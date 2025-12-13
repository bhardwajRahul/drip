package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

// TrafficStats tracks traffic statistics for a tunnel connection
type TrafficStats struct {
	// Total bytes
	totalBytesIn  int64
	totalBytesOut int64

	// Request counts
	totalRequests     int64
	activeConnections int64

	// For speed calculation
	lastBytesIn  int64
	lastBytesOut int64
	lastTime     time.Time
	speedMu      sync.Mutex

	// Current speed (bytes per second)
	speedIn  int64
	speedOut int64

	// Start time
	startTime time.Time
}

// NewTrafficStats creates a new traffic stats tracker
func NewTrafficStats() *TrafficStats {
	now := time.Now()
	return &TrafficStats{
		startTime: now,
		lastTime:  now,
	}
}

// AddBytesIn adds incoming bytes to the counter
func (s *TrafficStats) AddBytesIn(n int64) {
	atomic.AddInt64(&s.totalBytesIn, n)
}

// AddBytesOut adds outgoing bytes to the counter
func (s *TrafficStats) AddBytesOut(n int64) {
	atomic.AddInt64(&s.totalBytesOut, n)
}

// AddRequest increments the request counter
func (s *TrafficStats) AddRequest() {
	atomic.AddInt64(&s.totalRequests, 1)
}

func (s *TrafficStats) IncActiveConnections() {
	atomic.AddInt64(&s.activeConnections, 1)
}

func (s *TrafficStats) DecActiveConnections() {
	v := atomic.AddInt64(&s.activeConnections, -1)
	if v < 0 {
		atomic.StoreInt64(&s.activeConnections, 0)
	}
}

// GetTotalBytesIn returns total incoming bytes
func (s *TrafficStats) GetTotalBytesIn() int64 {
	return atomic.LoadInt64(&s.totalBytesIn)
}

// GetTotalBytesOut returns total outgoing bytes
func (s *TrafficStats) GetTotalBytesOut() int64 {
	return atomic.LoadInt64(&s.totalBytesOut)
}

// GetTotalRequests returns total request count
func (s *TrafficStats) GetTotalRequests() int64 {
	return atomic.LoadInt64(&s.totalRequests)
}

func (s *TrafficStats) GetActiveConnections() int64 {
	return atomic.LoadInt64(&s.activeConnections)
}

// GetTotalBytes returns total bytes (in + out)
func (s *TrafficStats) GetTotalBytes() int64 {
	return s.GetTotalBytesIn() + s.GetTotalBytesOut()
}

// UpdateSpeed calculates current transfer speed
// Should be called periodically (e.g., every second)
func (s *TrafficStats) UpdateSpeed() {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(s.lastTime).Seconds()

	// Require minimum interval of 100ms to avoid division issues
	if elapsed < 0.1 {
		return
	}

	currentIn := atomic.LoadInt64(&s.totalBytesIn)
	currentOut := atomic.LoadInt64(&s.totalBytesOut)

	deltaIn := currentIn - s.lastBytesIn
	deltaOut := currentOut - s.lastBytesOut

	// Calculate instantaneous speed
	if deltaIn > 0 {
		s.speedIn = int64(float64(deltaIn) / elapsed)
	} else {
		// No new bytes - set speed to 0 immediately
		s.speedIn = 0
	}

	if deltaOut > 0 {
		s.speedOut = int64(float64(deltaOut) / elapsed)
	} else {
		// No new bytes - set speed to 0 immediately
		s.speedOut = 0
	}

	s.lastBytesIn = currentIn
	s.lastBytesOut = currentOut
	s.lastTime = now
}

// GetSpeedIn returns current incoming speed in bytes per second
func (s *TrafficStats) GetSpeedIn() int64 {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	return s.speedIn
}

// GetSpeedOut returns current outgoing speed in bytes per second
func (s *TrafficStats) GetSpeedOut() int64 {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	return s.speedOut
}

// GetUptime returns how long the connection has been active
func (s *TrafficStats) GetUptime() time.Duration {
	return time.Since(s.startTime)
}

// Snapshot returns a snapshot of all stats
type Snapshot struct {
	TotalBytesIn      int64
	TotalBytesOut     int64
	TotalBytes        int64
	TotalRequests     int64
	ActiveConnections int64
	SpeedIn           int64 // bytes per second
	SpeedOut          int64 // bytes per second
	Uptime            time.Duration
}

// GetSnapshot returns a snapshot of current stats
func (s *TrafficStats) GetSnapshot() Snapshot {
	s.speedMu.Lock()
	speedIn := s.speedIn
	speedOut := s.speedOut
	s.speedMu.Unlock()

	totalIn := atomic.LoadInt64(&s.totalBytesIn)
	totalOut := atomic.LoadInt64(&s.totalBytesOut)
	active := atomic.LoadInt64(&s.activeConnections)

	return Snapshot{
		TotalBytesIn:      totalIn,
		TotalBytesOut:     totalOut,
		TotalBytes:        totalIn + totalOut,
		TotalRequests:     atomic.LoadInt64(&s.totalRequests),
		ActiveConnections: active,
		SpeedIn:           speedIn,
		SpeedOut:          speedOut,
		Uptime:            time.Since(s.startTime),
	}
}
