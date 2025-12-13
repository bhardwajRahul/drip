package tcp

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"drip/internal/server/tunnel"
	"drip/internal/shared/pool"
	"drip/internal/shared/recovery"

	"go.uber.org/zap"
)

// Listener handles TCP connections with TLS 1.3
type Listener struct {
	address     string
	tlsConfig   *tls.Config
	authToken   string
	manager     *tunnel.Manager
	portAlloc   *PortAllocator
	logger      *zap.Logger
	domain      string
	publicPort  int
	httpHandler http.Handler
	listener    net.Listener
	stopCh      chan struct{}
	wg          sync.WaitGroup
	connections map[string]*Connection
	connMu      sync.RWMutex
	workerPool  *pool.WorkerPool // Worker pool for connection handling
	recoverer   *recovery.Recoverer
	panicMetrics  *recovery.PanicMetrics

	groupManager *ConnectionGroupManager
}

func NewListener(address string, tlsConfig *tls.Config, authToken string, manager *tunnel.Manager, logger *zap.Logger, portAlloc *PortAllocator, domain string, publicPort int, httpHandler http.Handler) *Listener {
	numCPU := pool.NumCPU()
	workers := numCPU * 5
	queueSize := workers * 20
	workerPool := pool.NewWorkerPool(workers, queueSize)

	logger.Info("Worker pool configured",
		zap.Int("cpu_cores", numCPU),
		zap.Int("workers", workers),
		zap.Int("queue_size", queueSize),
	)

	panicMetrics := recovery.NewPanicMetrics(logger, nil)
	recoverer := recovery.NewRecoverer(logger, panicMetrics)

	return &Listener{
		address:      address,
		tlsConfig:    tlsConfig,
		authToken:    authToken,
		manager:      manager,
		portAlloc:    portAlloc,
		logger:       logger,
		domain:       domain,
		publicPort:   publicPort,
		httpHandler:  httpHandler,
		stopCh:       make(chan struct{}),
		connections:  make(map[string]*Connection),
		workerPool:   workerPool,
		recoverer:    recoverer,
		panicMetrics: panicMetrics,
		groupManager: NewConnectionGroupManager(logger),
	}
}

// Start starts the TCP listener
func (l *Listener) Start() error {
	var err error

	l.listener, err = tls.Listen("tcp", l.address, l.tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to start TLS listener: %w", err)
	}

	l.logger.Info("TCP listener started",
		zap.String("address", l.address),
		zap.String("tls_version", "TLS 1.3"),
	)

	l.wg.Add(1)
	go l.acceptLoop()

	return nil
}

// acceptLoop accepts incoming connections
func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	defer l.recoverer.Recover("acceptLoop")

	for {
		select {
		case <-l.stopCh:
			return
		default:
		}

		// Set accept deadline to allow checking stopCh
		if tcpListener, ok := l.listener.(*net.TCPListener); ok {
			tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := l.listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // Timeout is expected due to deadline
			}
			select {
			case <-l.stopCh:
				return
			default:
				l.logger.Error("Failed to accept connection", zap.Error(err))
				continue
			}
		}

		l.wg.Add(1)
		submitted := l.workerPool.Submit(l.recoverer.WrapGoroutine(
			fmt.Sprintf("handleConnection-%s", conn.RemoteAddr().String()),
			func() {
				l.handleConnection(conn)
			},
		))

		if !submitted {
			l.recoverer.SafeGo(
				fmt.Sprintf("handleConnection-fallback-%s", conn.RemoteAddr().String()),
				func() {
					l.handleConnection(conn)
				},
			)
		}
	}
}

// handleConnection handles a single client connection
func (l *Listener) handleConnection(netConn net.Conn) {
	defer l.wg.Done()
	defer netConn.Close()
	defer l.recoverer.RecoverWithCallback("handleConnection", func(p interface{}) {
		connID := netConn.RemoteAddr().String()
		l.connMu.Lock()
		delete(l.connections, connID)
		l.connMu.Unlock()
	})

	tlsConn, ok := netConn.(*tls.Conn)
	if !ok {
		l.logger.Error("Connection is not TLS")
		return
	}

	// Set read deadline before handshake to prevent slow handshake attacks
	if err := tlsConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		l.logger.Warn("Failed to set read deadline",
			zap.String("remote_addr", netConn.RemoteAddr().String()),
			zap.Error(err),
		)
		return
	}

	if err := tlsConn.Handshake(); err != nil {
		l.logger.Warn("TLS handshake failed",
			zap.String("remote_addr", netConn.RemoteAddr().String()),
			zap.Error(err),
		)
		return
	}

	// Clear the read deadline after successful handshake
	if err := tlsConn.SetReadDeadline(time.Time{}); err != nil {
		l.logger.Warn("Failed to clear read deadline",
			zap.String("remote_addr", netConn.RemoteAddr().String()),
			zap.Error(err),
		)
		return
	}

	if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetReadBuffer(256 * 1024)
		tcpConn.SetWriteBuffer(256 * 1024)
	}

	state := tlsConn.ConnectionState()
	l.logger.Info("New connection",
		zap.String("remote_addr", netConn.RemoteAddr().String()),
		zap.Uint16("tls_version", state.Version),
		zap.String("cipher_suite", tls.CipherSuiteName(state.CipherSuite)),
	)

	if state.Version != tls.VersionTLS13 {
		l.logger.Warn("Connection not using TLS 1.3",
			zap.Uint16("version", state.Version),
		)
		return
	}

	conn := NewConnection(netConn, l.authToken, l.manager, l.logger, l.portAlloc, l.domain, l.publicPort, l.httpHandler, l.groupManager)

	connID := netConn.RemoteAddr().String()
	l.connMu.Lock()
	l.connections[connID] = conn
	l.connMu.Unlock()

	defer func() {
		l.connMu.Lock()
		delete(l.connections, connID)
		l.connMu.Unlock()
	}()

	if err := conn.Handle(); err != nil {
		errStr := err.Error()

		// Client disconnection errors - normal network behavior, ignore
		if strings.Contains(errStr, "EOF") ||
			strings.Contains(errStr, "connection reset by peer") ||
			strings.Contains(errStr, "broken pipe") ||
			strings.Contains(errStr, "connection refused") {
			return
		}

		// Protocol errors (invalid clients, scanners) are expected - log as WARN
		if strings.Contains(errStr, "payload too large") ||
			strings.Contains(errStr, "failed to read registration frame") ||
			strings.Contains(errStr, "expected register frame") ||
			strings.Contains(errStr, "failed to parse registration request") ||
			strings.Contains(errStr, "failed to parse HTTP request") {
			l.logger.Warn("Protocol validation failed",
				zap.String("remote_addr", connID),
				zap.Error(err),
			)
		} else {
			// Legitimate errors (auth failures, registration failures, etc.)
			l.logger.Error("Connection handling failed",
				zap.String("remote_addr", connID),
				zap.Error(err),
			)
		}
	}
}

// Stop stops the listener and closes all connections
func (l *Listener) Stop() error {
	l.logger.Info("Stopping TCP listener")

	close(l.stopCh)

	if l.listener != nil {
		if err := l.listener.Close(); err != nil {
			l.logger.Error("Failed to close listener", zap.Error(err))
		}
	}

	l.connMu.Lock()
	for _, conn := range l.connections {
		conn.Close()
	}
	l.connMu.Unlock()

	l.wg.Wait()

	if l.workerPool != nil {
		l.workerPool.Close()
	}

	if l.groupManager != nil {
		l.groupManager.Close()
	}

	l.logger.Info("TCP listener stopped")
	return nil
}

// GetActiveConnections returns the number of active connections
func (l *Listener) GetActiveConnections() int {
	l.connMu.RLock()
	defer l.connMu.RUnlock()
	return len(l.connections)
}
