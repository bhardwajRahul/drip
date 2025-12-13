package tunnel

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/shared/protocol"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Connection represents a tunnel connection from a client
type Connection struct {
	Subdomain  string
	Conn       *websocket.Conn
	SendCh     chan []byte
	CloseCh    chan struct{}
	LastActive time.Time
	mu         sync.RWMutex
	logger     *zap.Logger
	closed     bool
	tunnelType protocol.TunnelType
	openStream func() (net.Conn, error)

	bytesIn           atomic.Int64
	bytesOut          atomic.Int64
	activeConnections atomic.Int64
}

// NewConnection creates a new tunnel connection
func NewConnection(subdomain string, conn *websocket.Conn, logger *zap.Logger) *Connection {
	return &Connection{
		Subdomain:  subdomain,
		Conn:       conn,
		SendCh:     make(chan []byte, 256),
		CloseCh:    make(chan struct{}),
		LastActive: time.Now(),
		logger:     logger,
		closed:     false,
	}
}

// Send sends data through the WebSocket connection
func (c *Connection) Send(data []byte) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrConnectionClosed
	}
	c.mu.RUnlock()

	select {
	case c.SendCh <- data:
		return nil
	case <-time.After(5 * time.Second):
		return ErrSendTimeout
	}
}

// UpdateActivity updates the last activity timestamp
func (c *Connection) UpdateActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastActive = time.Now()
}

// IsAlive checks if the connection is still alive based on last activity
func (c *Connection) IsAlive(timeout time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.LastActive) < timeout
}

// Close closes the connection and all associated channels
func (c *Connection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	c.closed = true
	close(c.CloseCh)
	close(c.SendCh)

	if c.Conn != nil {
		// Send close message
		c.Conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.Conn.Close()
	}

	c.logger.Info("Connection closed",
		zap.String("subdomain", c.Subdomain),
	)
}

// IsClosed returns whether the connection is closed
func (c *Connection) IsClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

// SetTunnelType sets the tunnel type.
func (c *Connection) SetTunnelType(tType protocol.TunnelType) {
	c.mu.Lock()
	c.tunnelType = tType
	c.mu.Unlock()
}

// GetTunnelType returns the tunnel type.
func (c *Connection) GetTunnelType() protocol.TunnelType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tunnelType
}

// SetOpenStream registers a yamux stream opener for this tunnel.
// It is used by the HTTP proxy to forward each request over a mux stream.
func (c *Connection) SetOpenStream(open func() (net.Conn, error)) {
	c.mu.Lock()
	c.openStream = open
	c.mu.Unlock()
}

// OpenStream opens a new mux stream to the tunnel client.
func (c *Connection) OpenStream() (net.Conn, error) {
	c.mu.RLock()
	open := c.openStream
	closed := c.closed
	c.mu.RUnlock()

	if closed || open == nil {
		return nil, ErrConnectionClosed
	}
	return open()
}

func (c *Connection) AddBytesIn(n int64) {
	if n <= 0 {
		return
	}
	c.bytesIn.Add(n)
}

func (c *Connection) AddBytesOut(n int64) {
	if n <= 0 {
		return
	}
	c.bytesOut.Add(n)
}

func (c *Connection) GetBytesIn() int64 {
	return c.bytesIn.Load()
}

func (c *Connection) GetBytesOut() int64 {
	return c.bytesOut.Load()
}

func (c *Connection) IncActiveConnections() {
	c.activeConnections.Add(1)
}

func (c *Connection) DecActiveConnections() {
	if v := c.activeConnections.Add(-1); v < 0 {
		c.activeConnections.Store(0)
	}
}

func (c *Connection) GetActiveConnections() int64 {
	return c.activeConnections.Load()
}

// StartWritePump starts the write pump for sending messages
func (c *Connection) StartWritePump() {
	// Skip write pump for TCP-only connections (no WebSocket)
	if c.Conn == nil {
		c.logger.Debug("Skipping WritePump for TCP connection",
			zap.String("subdomain", c.Subdomain),
		)
		// Still need to drain SendCh to prevent blocking
		go func() {
			for {
				select {
				case <-c.SendCh:
					// Discard messages for TCP mode
				case <-c.CloseCh:
					return
				}
			}
		}()
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.Close()
	}()

	for {
		select {
		case message, ok := <-c.SendCh:
			if !ok {
				return
			}

			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				c.logger.Error("Write error",
					zap.String("subdomain", c.Subdomain),
					zap.Error(err),
				)
				return
			}

		case <-ticker.C:
			// Send ping to keep connection alive
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.CloseCh:
			return
		}
	}
}
