package tunnel

import (
	"errors"
	"sync"
	"time"

	"drip/internal/shared/utils"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Manager limits
const (
	DefaultMaxTunnels      = 1000             // Maximum total tunnels
	DefaultMaxTunnelsPerIP = 10               // Maximum tunnels per IP
	DefaultRateLimit       = 10               // Registrations per IP per minute
	DefaultRateLimitWindow = 1 * time.Minute  // Rate limit window
)

var (
	ErrTooManyTunnels    = errors.New("maximum tunnel limit reached")
	ErrTooManyPerIP      = errors.New("maximum tunnels per IP reached")
	ErrRateLimitExceeded = errors.New("rate limit exceeded, try again later")
)

// rateLimitEntry tracks registration attempts per IP
type rateLimitEntry struct {
	count     int
	windowEnd time.Time
}

// Manager manages all active tunnel connections
type Manager struct {
	tunnels map[string]*Connection // subdomain -> connection
	mu      sync.RWMutex
	used    map[string]bool // track used subdomains
	logger  *zap.Logger

	// Limits
	maxTunnels      int
	maxTunnelsPerIP int
	rateLimit       int
	rateLimitWindow time.Duration

	// Per-IP tracking
	tunnelsByIP map[string]int             // IP -> tunnel count
	rateLimits  map[string]*rateLimitEntry // IP -> rate limit entry

	// Lifecycle
	stopCh chan struct{}
}

// ManagerConfig holds configuration for the Manager
type ManagerConfig struct {
	MaxTunnels      int
	MaxTunnelsPerIP int
	RateLimit       int           // Registrations per IP per window
	RateLimitWindow time.Duration
}

// DefaultManagerConfig returns default configuration
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxTunnels:      DefaultMaxTunnels,
		MaxTunnelsPerIP: DefaultMaxTunnelsPerIP,
		RateLimit:       DefaultRateLimit,
		RateLimitWindow: DefaultRateLimitWindow,
	}
}

// NewManager creates a new tunnel manager with default config
func NewManager(logger *zap.Logger) *Manager {
	return NewManagerWithConfig(logger, DefaultManagerConfig())
}

// NewManagerWithConfig creates a new tunnel manager with custom config
func NewManagerWithConfig(logger *zap.Logger, cfg ManagerConfig) *Manager {
	if cfg.MaxTunnels <= 0 {
		cfg.MaxTunnels = DefaultMaxTunnels
	}
	if cfg.MaxTunnelsPerIP <= 0 {
		cfg.MaxTunnelsPerIP = DefaultMaxTunnelsPerIP
	}
	if cfg.RateLimit <= 0 {
		cfg.RateLimit = DefaultRateLimit
	}
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = DefaultRateLimitWindow
	}

	logger.Info("Tunnel manager configured",
		zap.Int("max_tunnels", cfg.MaxTunnels),
		zap.Int("max_per_ip", cfg.MaxTunnelsPerIP),
		zap.Int("rate_limit", cfg.RateLimit),
		zap.Duration("rate_window", cfg.RateLimitWindow),
	)

	return &Manager{
		tunnels:         make(map[string]*Connection),
		used:            make(map[string]bool),
		logger:          logger,
		maxTunnels:      cfg.MaxTunnels,
		maxTunnelsPerIP: cfg.MaxTunnelsPerIP,
		rateLimit:       cfg.RateLimit,
		rateLimitWindow: cfg.RateLimitWindow,
		tunnelsByIP:     make(map[string]int),
		rateLimits:      make(map[string]*rateLimitEntry),
		stopCh:          make(chan struct{}),
	}
}

// checkRateLimit checks if the IP has exceeded rate limit
func (m *Manager) checkRateLimit(ip string) bool {
	now := time.Now()
	entry, exists := m.rateLimits[ip]

	if !exists || now.After(entry.windowEnd) {
		// New window
		m.rateLimits[ip] = &rateLimitEntry{
			count:     1,
			windowEnd: now.Add(m.rateLimitWindow),
		}
		return true
	}

	if entry.count >= m.rateLimit {
		return false
	}

	entry.count++
	return true
}

// Register registers a new tunnel connection with IP-based limits
func (m *Manager) Register(conn *websocket.Conn, customSubdomain string) (string, error) {
	return m.RegisterWithIP(conn, customSubdomain, "")
}

// RegisterWithIP registers a new tunnel with IP tracking
func (m *Manager) RegisterWithIP(conn *websocket.Conn, customSubdomain string, remoteIP string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check total tunnel limit
	if len(m.tunnels) >= m.maxTunnels {
		m.logger.Warn("Maximum tunnel limit reached",
			zap.Int("current", len(m.tunnels)),
			zap.Int("max", m.maxTunnels),
		)
		return "", ErrTooManyTunnels
	}

	// Check per-IP limits if IP is provided
	if remoteIP != "" {
		// Check rate limit
		if !m.checkRateLimit(remoteIP) {
			m.logger.Warn("Rate limit exceeded",
				zap.String("ip", remoteIP),
				zap.Int("limit", m.rateLimit),
			)
			return "", ErrRateLimitExceeded
		}

		// Check per-IP tunnel limit
		if m.tunnelsByIP[remoteIP] >= m.maxTunnelsPerIP {
			m.logger.Warn("Per-IP tunnel limit reached",
				zap.String("ip", remoteIP),
				zap.Int("current", m.tunnelsByIP[remoteIP]),
				zap.Int("max", m.maxTunnelsPerIP),
			)
			return "", ErrTooManyPerIP
		}
	}

	var subdomain string

	if customSubdomain != "" {
		// Validate custom subdomain
		if !utils.ValidateSubdomain(customSubdomain) {
			return "", ErrInvalidSubdomain
		}
		if utils.IsReserved(customSubdomain) {
			return "", ErrReservedSubdomain
		}
		if m.used[customSubdomain] {
			return "", ErrSubdomainTaken
		}
		subdomain = customSubdomain
	} else {
		// Generate unique random subdomain
		subdomain = m.generateUniqueSubdomain()
	}

	// Create connection
	tc := NewConnection(subdomain, conn, m.logger)
	tc.remoteIP = remoteIP // Track IP for cleanup
	m.tunnels[subdomain] = tc
	m.used[subdomain] = true

	// Update per-IP counter
	if remoteIP != "" {
		m.tunnelsByIP[remoteIP]++
	}

	// Start write pump in background
	go tc.StartWritePump()

	m.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("ip", remoteIP),
		zap.Int("total_tunnels", len(m.tunnels)),
	)

	return subdomain, nil
}

// Unregister removes a tunnel connection
func (m *Manager) Unregister(subdomain string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if tc, ok := m.tunnels[subdomain]; ok {
		// Decrement per-IP counter
		if tc.remoteIP != "" && m.tunnelsByIP[tc.remoteIP] > 0 {
			m.tunnelsByIP[tc.remoteIP]--
			if m.tunnelsByIP[tc.remoteIP] == 0 {
				delete(m.tunnelsByIP, tc.remoteIP)
			}
		}

		tc.Close()
		delete(m.tunnels, subdomain)
		delete(m.used, subdomain)

		m.logger.Info("Tunnel unregistered",
			zap.String("subdomain", subdomain),
			zap.Int("total_tunnels", len(m.tunnels)),
		)
	}
}

// Get retrieves a tunnel connection by subdomain
func (m *Manager) Get(subdomain string) (*Connection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tc, ok := m.tunnels[subdomain]
	return tc, ok
}

// List returns all active tunnel connections
func (m *Manager) List() []*Connection {
	m.mu.RLock()
	defer m.mu.RUnlock()

	connections := make([]*Connection, 0, len(m.tunnels))
	for _, tc := range m.tunnels {
		connections = append(connections, tc)
	}
	return connections
}

// Count returns the number of active tunnels
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tunnels)
}

// CleanupStale removes stale connections that haven't been active
func (m *Manager) CleanupStale(timeout time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	staleSubdomains := []string{}

	for subdomain, tc := range m.tunnels {
		if !tc.IsAlive(timeout) {
			staleSubdomains = append(staleSubdomains, subdomain)
		}
	}

	for _, subdomain := range staleSubdomains {
		if tc, ok := m.tunnels[subdomain]; ok {
			// Decrement per-IP counter
			if tc.remoteIP != "" && m.tunnelsByIP[tc.remoteIP] > 0 {
				m.tunnelsByIP[tc.remoteIP]--
				if m.tunnelsByIP[tc.remoteIP] == 0 {
					delete(m.tunnelsByIP, tc.remoteIP)
				}
			}

			tc.Close()
			delete(m.tunnels, subdomain)
			delete(m.used, subdomain)
		}
	}

	// Cleanup expired rate limit entries
	now := time.Now()
	for ip, entry := range m.rateLimits {
		if now.After(entry.windowEnd) {
			delete(m.rateLimits, ip)
		}
	}

	if len(staleSubdomains) > 0 {
		m.logger.Info("Cleaned up stale tunnels",
			zap.Int("count", len(staleSubdomains)),
		)
	}

	return len(staleSubdomains)
}

// StartCleanupTask starts a background task to clean up stale connections
func (m *Manager) StartCleanupTask(interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.CleanupStale(timeout)
			case <-m.stopCh:
				return
			}
		}
	}()
}

// generateUniqueSubdomain generates a unique random subdomain
func (m *Manager) generateUniqueSubdomain() string {
	const maxAttempts = 10

	for i := 0; i < maxAttempts; i++ {
		subdomain := utils.GenerateSubdomain(6)
		if !m.used[subdomain] && !utils.IsReserved(subdomain) {
			return subdomain
		}
	}

	// Fallback: use longer subdomain if collision persists
	return utils.GenerateSubdomain(8)
}

// Shutdown gracefully shuts down all tunnels
func (m *Manager) Shutdown() {
	// Signal cleanup goroutine to stop
	close(m.stopCh)

	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info("Shutting down tunnel manager",
		zap.Int("active_tunnels", len(m.tunnels)),
	)

	for _, tc := range m.tunnels {
		tc.Close()
	}

	m.tunnels = make(map[string]*Connection)
	m.used = make(map[string]bool)
}
