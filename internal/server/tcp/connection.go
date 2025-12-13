package tcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"

	"drip/internal/server/tunnel"
	"drip/internal/shared/constants"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

// Connection represents a client TCP connection
type Connection struct {
	conn          net.Conn
	authToken     string
	manager       *tunnel.Manager
	logger        *zap.Logger
	subdomain     string
	port          int
	domain        string
	publicPort    int
	portAlloc     *PortAllocator
	tunnelConn    *tunnel.Connection
	stopCh        chan struct{}
	once          sync.Once
	lastHeartbeat time.Time
	mu            sync.RWMutex
	frameWriter   *protocol.FrameWriter
	httpHandler   http.Handler
	tunnelType    protocol.TunnelType // Track tunnel type
	ctx           context.Context
	cancel        context.CancelFunc

	// gost-like TCP tunnel (yamux)
	session *yamux.Session
	proxy   *Proxy

	// Multi-connection support
	tunnelID     string
	groupManager *ConnectionGroupManager
}

// NewConnection creates a new connection handler
func NewConnection(conn net.Conn, authToken string, manager *tunnel.Manager, logger *zap.Logger, portAlloc *PortAllocator, domain string, publicPort int, httpHandler http.Handler, groupManager *ConnectionGroupManager) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Connection{
		conn:          conn,
		authToken:     authToken,
		manager:       manager,
		logger:        logger,
		portAlloc:     portAlloc,
		domain:        domain,
		publicPort:    publicPort,
		httpHandler:   httpHandler,
		stopCh:        make(chan struct{}),
		lastHeartbeat: time.Now(),
		ctx:           ctx,
		cancel:        cancel,
		groupManager:  groupManager,
	}
	return c
}

// Handle handles the connection lifecycle
func (c *Connection) Handle() error {
	// Register connection for adaptive load tracking
	protocol.RegisterConnection()

	// Ensure cleanup of control connection, proxy, port, and registry on exit.
	defer c.Close()

	// Set initial read timeout for protocol detection
	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Use buffered reader to support peeking
	reader := bufio.NewReader(c.conn)

	// Peek first 4 bytes to detect protocol (HTTP methods are 4 bytes).
	peek, err := reader.Peek(4)
	if err != nil {
		return fmt.Errorf("failed to peek connection: %w", err)
	}

	peekStr := string(peek)
	httpMethods := []string{"GET ", "POST", "PUT ", "DELE", "HEAD", "OPTI", "PATC", "CONN", "TRAC"}
	isHTTP := false
	for _, method := range httpMethods {
		if strings.HasPrefix(peekStr, method) {
			isHTTP = true
			break
		}
	}

	if isHTTP {
		c.logger.Info("Detected HTTP request on TCP port, handling as HTTP")
		return c.handleHTTPRequest(reader)
	}

	// Continue with drip protocol
	// Wait for registration frame
	frame, err := protocol.ReadFrame(reader)
	if err != nil {
		return fmt.Errorf("failed to read registration frame: %w", err)
	}
	sf := protocol.WithFrame(frame)
	defer sf.Close()

	// Handle data connection request (for multi-connection pool)
	if sf.Frame.Type == protocol.FrameTypeDataConnect {
		return c.handleDataConnect(sf.Frame, reader)
	}

	if sf.Frame.Type != protocol.FrameTypeRegister {
		return fmt.Errorf("expected register frame, got %s", sf.Frame.Type)
	}

	var req protocol.RegisterRequest
	if err := json.Unmarshal(sf.Frame.Payload, &req); err != nil {
		return fmt.Errorf("failed to parse registration request: %w", err)
	}

	c.tunnelType = req.TunnelType

	if c.authToken != "" && req.Token != c.authToken {
		c.sendError("authentication_failed", "Invalid authentication token")
		return fmt.Errorf("authentication failed")
	}

	// Allocate TCP port only for TCP tunnels
	if req.TunnelType == protocol.TunnelTypeTCP {
		if c.portAlloc == nil {
			return fmt.Errorf("port allocator not configured")
		}

		port, err := c.portAlloc.Allocate()
		if err != nil {
			c.sendError("port_allocation_failed", err.Error())
			return fmt.Errorf("failed to allocate port: %w", err)
		}
		c.port = port

		// For TCP tunnels, prefer deterministic subdomain tied to port when not provided by client.
		if req.CustomSubdomain == "" {
			req.CustomSubdomain = fmt.Sprintf("tcp-%d", port)
		}
	}

	subdomain, err := c.manager.Register(nil, req.CustomSubdomain)
	if err != nil {
		c.sendError("registration_failed", err.Error())
		c.portAlloc.Release(c.port)
		c.port = 0
		return fmt.Errorf("tunnel registration failed: %w", err)
	}

	c.subdomain = subdomain

	tunnelConn, ok := c.manager.Get(subdomain)
	if !ok {
		return fmt.Errorf("failed to get registered tunnel")
	}
	c.tunnelConn = tunnelConn

	// Store TCP connection reference and metadata for HTTP proxy routing
	c.tunnelConn.Conn = nil // We're using TCP, not WebSocket
	c.tunnelConn.SetTunnelType(req.TunnelType)
	c.tunnelType = req.TunnelType

	c.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("tunnel_type", string(req.TunnelType)),
		zap.Int("local_port", req.LocalPort),
		zap.Int("remote_port", c.port),
	)

	// Send registration acknowledgment
	// Generate appropriate URL based on tunnel type
	var tunnelURL string

	if req.TunnelType == protocol.TunnelTypeHTTP || req.TunnelType == protocol.TunnelTypeHTTPS {
		// HTTP/HTTPS tunnels use HTTPS with subdomain
		// Use publicPort for URL generation (configured via --public-port flag)
		if c.publicPort == 443 {
			tunnelURL = fmt.Sprintf("https://%s.%s", subdomain, c.domain)
		} else {
			tunnelURL = fmt.Sprintf("https://%s.%s:%d", subdomain, c.domain, c.publicPort)
		}
	} else {
		// TCP tunnels use tcp:// with port
		tunnelURL = fmt.Sprintf("tcp://%s:%d", c.domain, c.port)
	}

	// Generate TunnelID for multi-connection support if client supports it
	var tunnelID string
	var supportsDataConn bool
	recommendedConns := 0

	if req.PoolCapabilities != nil && req.ConnectionType == "primary" && c.groupManager != nil {
		// Client supports connection pooling
		group := c.groupManager.CreateGroup(subdomain, req.Token, c, req.TunnelType)
		tunnelID = group.TunnelID
		c.tunnelID = tunnelID
		supportsDataConn = true
		recommendedConns = 4 // Recommend 4 data connections

		c.logger.Info("Created connection group for multi-connection support",
			zap.String("tunnel_id", tunnelID),
			zap.Int("max_data_conns", req.PoolCapabilities.MaxDataConns),
		)
	}

	resp := protocol.RegisterResponse{
		Subdomain:        subdomain,
		Port:             c.port,
		URL:              tunnelURL,
		Message:          "Tunnel registered successfully",
		TunnelID:         tunnelID,
		SupportsDataConn: supportsDataConn,
		RecommendedConns: recommendedConns,
	}

	respData, _ := json.Marshal(resp)
	ackFrame := protocol.NewFrame(protocol.FrameTypeRegisterAck, respData)

	// Send registration ack (sync write before frameWriter is created)
	err = protocol.WriteFrame(c.conn, ackFrame)
	if err != nil {
		return fmt.Errorf("failed to send registration ack: %w", err)
	}

	// Clear deadline for tunnel data-plane.
	c.conn.SetReadDeadline(time.Time{})

	// gost-like tunnels: switch to yamux after RegisterAck.
	if req.TunnelType == protocol.TunnelTypeTCP {
		return c.handleTCPTunnel(reader)
	}
	if req.TunnelType == protocol.TunnelTypeHTTP || req.TunnelType == protocol.TunnelTypeHTTPS {
		return c.handleHTTPProxyTunnel(reader)
	}

	c.frameWriter = protocol.NewFrameWriter(c.conn)

	c.frameWriter.SetWriteErrorHandler(func(err error) {
		c.logger.Error("Write error detected, closing connection", zap.Error(err))
		c.Close()
	})

	go c.heartbeatChecker()

	return c.handleFrames(reader)
}

func (c *Connection) handleHTTPRequest(reader *bufio.Reader) error {
	if c.httpHandler == nil {
		c.logger.Warn("HTTP request received but no HTTP handler configured")
		response := "HTTP/1.1 503 Service Unavailable\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 47\r\n" +
			"\r\n" +
			"HTTP handler not configured for this TCP port\r\n"
		c.conn.Write([]byte(response))
		return fmt.Errorf("HTTP handler not configured")
	}

	// Clear read deadline for HTTP processing
	c.conn.SetReadDeadline(time.Time{})

	// Handle multiple HTTP requests on the same connection (HTTP/1.1 keep-alive)
	for {
		// Set a read deadline for each request to avoid hanging forever
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Parse HTTP request
		req, err := http.ReadRequest(reader)
		if err != nil {
			// EOF or timeout is normal when client closes connection or no more requests
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				c.logger.Debug("Client closed HTTP connection")
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				c.logger.Debug("HTTP keep-alive timeout")
				return nil
			}
			// Connection reset by peer is normal - client closed connection abruptly
			errStr := err.Error()
			if errors.Is(err, net.ErrClosed) || strings.Contains(errStr, "use of closed network connection") {
				c.logger.Debug("HTTP connection closed during read", zap.Error(err))
				return nil
			}
			if strings.Contains(errStr, "connection reset by peer") ||
				strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "connection refused") {
				c.logger.Debug("Client disconnected abruptly", zap.Error(err))
				return nil
			}
			// Check if it looks like garbage data (not a valid HTTP request)
			if strings.Contains(errStr, "malformed HTTP") {
				c.logger.Warn("Received malformed HTTP request, possibly due to pipelined requests or protocol error",
					zap.Error(err),
					zap.String("error_snippet", errStr[:min(len(errStr), 100)]),
				)
				// Close connection on malformed request to prevent further errors
				return nil
			}
			c.logger.Error("Failed to parse HTTP request", zap.Error(err))
			return fmt.Errorf("failed to parse HTTP request: %w", err)
		}

		if c.ctx != nil {
			req = req.WithContext(c.ctx)
		}

		c.logger.Info("Processing HTTP request on TCP port",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
			zap.String("host", req.Host),
		)

		respWriter := &httpResponseWriter{
			conn:   c.conn,
			writer: bufio.NewWriterSize(c.conn, 4096),
			header: make(http.Header),
		}

		c.httpHandler.ServeHTTP(respWriter, req)

		if err := respWriter.writer.Flush(); err != nil {
			c.logger.Debug("Failed to flush HTTP response", zap.Error(err))
		}

		if tcpConn, ok := c.conn.(*net.TCPConn); ok {
			tcpConn.SetNoDelay(true)
			tcpConn.SetNoDelay(false)
		}

		c.logger.Debug("HTTP request processing completed",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
		)

		shouldClose := false
		if req.Close {
			shouldClose = true
		} else if req.ProtoMajor == 1 && req.ProtoMinor == 0 {
			if req.Header.Get("Connection") != "keep-alive" {
				shouldClose = true
			}
		}

		if respWriter.headerWritten && respWriter.header.Get("Connection") == "close" {
			shouldClose = true
		}

		if shouldClose {
			c.logger.Debug("Closing connection as requested by client or server")
			return nil
		}

		// Continue to next request on the same connection
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleFrames handles incoming frames
func (c *Connection) handleFrames(reader *bufio.Reader) error {
	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		// Read frame with timeout
		c.conn.SetReadDeadline(time.Now().Add(constants.RequestTimeout))
		frame, err := protocol.ReadFrame(reader)
		if err != nil {
			if isTimeoutError(err) {
				c.logger.Warn("Read timeout, connection may be dead")
				return fmt.Errorf("read timeout")
			}
			// EOF is normal when client closes connection gracefully
			if err.Error() == "failed to read frame header: EOF" || err.Error() == "EOF" {
				c.logger.Info("Client disconnected")
				return nil
			}
			// Check if connection was closed (during shutdown)
			select {
			case <-c.stopCh:
				// Connection was closed intentionally, don't log as error
				c.logger.Debug("Connection closed during shutdown")
				return nil
			default:
				return fmt.Errorf("failed to read frame: %w", err)
			}
		}

		// Handle frame based on type
		sf := protocol.WithFrame(frame)

		switch sf.Frame.Type {
		case protocol.FrameTypeHeartbeat:
			c.handleHeartbeat()
			sf.Close()

		case protocol.FrameTypeClose:
			sf.Close()
			c.logger.Info("Client requested close")
			return nil

		default:
			sf.Close()
			c.logger.Warn("Unexpected frame type",
				zap.String("type", sf.Frame.Type.String()),
			)
		}
	}
}

// handleHeartbeat handles heartbeat frame
func (c *Connection) handleHeartbeat() {
	c.mu.Lock()
	c.lastHeartbeat = time.Now()
	c.mu.Unlock()

	// Send heartbeat ack
	ackFrame := protocol.NewFrame(protocol.FrameTypeHeartbeatAck, nil)

	err := c.frameWriter.WriteControl(ackFrame)
	if err != nil {
		c.logger.Error("Failed to send heartbeat ack", zap.Error(err))
	}
}

// heartbeatChecker checks for heartbeat timeout
func (c *Connection) heartbeatChecker() {
	ticker := time.NewTicker(constants.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.RLock()
			lastHB := c.lastHeartbeat
			c.mu.RUnlock()

			if time.Since(lastHB) > constants.HeartbeatTimeout {
				c.logger.Warn("Heartbeat timeout",
					zap.String("subdomain", c.subdomain),
					zap.Duration("last_heartbeat", time.Since(lastHB)),
				)
				c.Close()
				return
			}
		}
	}
}

func (c *Connection) sendError(code, message string) {
	errMsg := protocol.ErrorMessage{
		Code:    code,
		Message: message,
	}
	data, _ := json.Marshal(errMsg)
	errFrame := protocol.NewFrame(protocol.FrameTypeError, data)

	if c.frameWriter == nil {
		protocol.WriteFrame(c.conn, errFrame)
	} else {
		c.frameWriter.WriteFrame(errFrame)
	}
}

func (c *Connection) Close() {
	c.once.Do(func() {
		protocol.UnregisterConnection()

		close(c.stopCh)

		if c.cancel != nil {
			c.cancel()
		}

		// Ensure any in-flight writes return quickly on shutdown to avoid hanging.
		if c.conn != nil {
			_ = c.conn.SetDeadline(time.Now())
		}

		if c.frameWriter != nil {
			c.frameWriter.Close()
		}

		if c.proxy != nil {
			c.proxy.Stop()
		}

		if c.session != nil {
			_ = c.session.Close()
		}

		if c.conn != nil {
			c.conn.Close()
		}

		if c.port > 0 && c.portAlloc != nil {
			c.portAlloc.Release(c.port)
		}

		if c.subdomain != "" {
			c.manager.Unregister(c.subdomain)

			// Clean up connection group when PRIMARY connection closes
			// (only primary connections have subdomain set)
			if c.tunnelID != "" && c.groupManager != nil {
				c.groupManager.RemoveGroup(c.tunnelID)
			}
		}

		c.logger.Info("Connection closed",
			zap.String("subdomain", c.subdomain),
		)
	})
}

// httpResponseWriter implements http.ResponseWriter for writing to a net.Conn
type httpResponseWriter struct {
	conn          net.Conn
	writer        *bufio.Writer // Buffered writer for efficient I/O
	header        http.Header
	statusCode    int
	headerWritten bool
}

func (w *httpResponseWriter) Header() http.Header {
	return w.header
}

func (w *httpResponseWriter) WriteHeader(statusCode int) {
	if w.headerWritten {
		return
	}
	w.statusCode = statusCode
	w.headerWritten = true

	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown"
	}

	w.writer.WriteString("HTTP/1.1 ")
	w.writer.WriteString(fmt.Sprintf("%d", statusCode))
	w.writer.WriteByte(' ')
	w.writer.WriteString(statusText)
	w.writer.WriteString("\r\n")

	for key, values := range w.header {
		for _, value := range values {
			w.writer.WriteString(key)
			w.writer.WriteString(": ")
			w.writer.WriteString(value)
			w.writer.WriteString("\r\n")
		}
	}

	w.writer.WriteString("\r\n")
}

func (w *httpResponseWriter) Write(data []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	return w.writer.Write(data)
}

// handleDataConnect handles a data connection join request
func (c *Connection) handleDataConnect(frame *protocol.Frame, reader *bufio.Reader) error {
	var req protocol.DataConnectRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		c.sendError("invalid_request", "Failed to parse data connect request")
		return fmt.Errorf("failed to parse data connect request: %w", err)
	}

	c.logger.Info("Data connection request received",
		zap.String("tunnel_id", req.TunnelID),
		zap.String("connection_id", req.ConnectionID),
	)

	// Validate the request
	if c.groupManager == nil {
		c.sendDataConnectError("not_supported", "Multi-connection not supported")
		return fmt.Errorf("group manager not available")
	}

	// Validate auth token
	if c.authToken != "" && req.Token != c.authToken {
		c.sendDataConnectError("authentication_failed", "Invalid authentication token")
		return fmt.Errorf("authentication failed for data connection")
	}

	group, ok := c.groupManager.GetGroup(req.TunnelID)
	if !ok || group == nil {
		c.sendDataConnectError("join_failed", "Tunnel not found")
		return fmt.Errorf("tunnel not found: %s", req.TunnelID)
	}

	// Validate token against the primary registration token.
	if group.Token != "" && req.Token != group.Token {
		c.sendDataConnectError("authentication_failed", "Invalid authentication token")
		return fmt.Errorf("authentication failed for data connection")
	}

	// Store tunnelID for cleanup
	c.tunnelID = req.TunnelID

	// For TCP tunnels, the data connection is upgraded to a yamux session and used for
	// stream forwarding, not framed request/response routing.
	if group.TunnelType == protocol.TunnelTypeTCP {
		resp := protocol.DataConnectResponse{
			Accepted:     true,
			ConnectionID: req.ConnectionID,
			Message:      "Data connection accepted",
		}

		respData, _ := json.Marshal(resp)
		ackFrame := protocol.NewFrame(protocol.FrameTypeDataConnectAck, respData)

		if err := protocol.WriteFrame(c.conn, ackFrame); err != nil {
			return fmt.Errorf("failed to send data connect ack: %w", err)
		}

		c.logger.Info("TCP data connection established",
			zap.String("tunnel_id", req.TunnelID),
			zap.String("connection_id", req.ConnectionID),
		)

		// Clear deadline for yamux data-plane.
		_ = c.conn.SetReadDeadline(time.Time{})

		// Public server acts as yamux Client, client connector acts as yamux Server.
		bc := &bufferedConn{
			Conn:   c.conn,
			reader: reader,
		}

		cfg := yamux.DefaultConfig()
		cfg.EnableKeepAlive = false
		cfg.LogOutput = io.Discard
		cfg.AcceptBacklog = constants.YamuxAcceptBacklog

		session, err := yamux.Client(bc, cfg)
		if err != nil {
			return fmt.Errorf("failed to init yamux session: %w", err)
		}
		c.session = session

		group.AddSession(req.ConnectionID, session)
		defer group.RemoveSession(req.ConnectionID)

		select {
		case <-c.stopCh:
			return nil
		case <-session.CloseChan():
			return nil
		}
	}

	// Add data connection to group
	dataConn, err := c.groupManager.AddDataConnection(&req, c.conn)
	if err != nil {
		c.sendDataConnectError("join_failed", err.Error())
		return fmt.Errorf("failed to join connection group: %w", err)
	}

	// Send success response
	resp := protocol.DataConnectResponse{
		Accepted:     true,
		ConnectionID: req.ConnectionID,
		Message:      "Data connection accepted",
	}

	respData, _ := json.Marshal(resp)
	ackFrame := protocol.NewFrame(protocol.FrameTypeDataConnectAck, respData)

	if err := protocol.WriteFrame(c.conn, ackFrame); err != nil {
		return fmt.Errorf("failed to send data connect ack: %w", err)
	}

	c.logger.Info("Data connection established",
		zap.String("tunnel_id", req.TunnelID),
		zap.String("connection_id", req.ConnectionID),
	)

	// Handle data frames on this connection
	return c.handleDataConnectionFrames(dataConn, reader)
}

// handleDataConnectionFrames handles frames on a data connection
func (c *Connection) handleDataConnectionFrames(dataConn *DataConnection, reader *bufio.Reader) error {
	defer func() {
		// Get the group and remove this data connection
		if group, ok := c.groupManager.GetGroup(c.tunnelID); ok {
			group.RemoveDataConnection(dataConn.ID)
		}
	}()

	for {
		select {
		case <-dataConn.stopCh:
			return nil
		default:
		}

		c.conn.SetReadDeadline(time.Now().Add(constants.RequestTimeout))
		frame, err := protocol.ReadFrame(reader)
		if err != nil {
			// Timeout is OK, continue
			if isTimeoutError(err) {
				continue
			}
			return err
		}

		dataConn.mu.Lock()
		dataConn.LastActive = time.Now()
		dataConn.mu.Unlock()

		sf := protocol.WithFrame(frame)

		switch sf.Frame.Type {
		case protocol.FrameTypeClose:
			sf.Close()
			c.logger.Info("Data connection closed by client",
				zap.String("connection_id", dataConn.ID))
			return nil

		default:
			sf.Close()
			c.logger.Warn("Unexpected frame type on data connection",
				zap.String("type", sf.Frame.Type.String()),
				zap.String("connection_id", dataConn.ID),
			)
		}
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Fallback for wrapped errors without net.Error
	return strings.Contains(err.Error(), "i/o timeout")
}

// sendDataConnectError sends a data connect error response
func (c *Connection) sendDataConnectError(code, message string) {
	resp := protocol.DataConnectResponse{
		Accepted: false,
		Message:  fmt.Sprintf("%s: %s", code, message),
	}
	respData, _ := json.Marshal(resp)
	frame := protocol.NewFrame(protocol.FrameTypeDataConnectAck, respData)
	protocol.WriteFrame(c.conn, frame)
}
