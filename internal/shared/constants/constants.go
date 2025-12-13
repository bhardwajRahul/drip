package constants

import "time"

const (
	// DefaultServerPort is the default port for the tunnel server
	DefaultServerPort = 8080

	// DefaultWSPort is the default WebSocket port
	DefaultWSPort = 8080

	// YamuxAcceptBacklog controls how many incoming streams can be queued
	// before yamux starts blocking stream opens under load.
	YamuxAcceptBacklog = 4096

	// HeartbeatInterval is how often clients send heartbeat messages
	HeartbeatInterval = 2 * time.Second

	// HeartbeatTimeout is how long the server waits before considering a connection dead
	HeartbeatTimeout = 6 * time.Second

	// RequestTimeout is the maximum time to wait for a response from the client
	RequestTimeout = 30 * time.Second

	// ReconnectBaseDelay is the initial delay for reconnection attempts
	ReconnectBaseDelay = 1 * time.Second

	// ReconnectMaxDelay is the maximum delay between reconnection attempts
	ReconnectMaxDelay = 60 * time.Second

	// MaxReconnectAttempts is the maximum number of reconnection attempts (0 = infinite)
	MaxReconnectAttempts = 0

	// DefaultTCPPortMin/Max define the default allocation range for TCP tunnels
	DefaultTCPPortMin = 20000
	DefaultTCPPortMax = 40000
	// DefaultDomain is the default domain for tunnels
	DefaultDomain = "tunnel.localhost"
)

// Error codes
const (
	ErrCodeTunnelNotFound   = "TUNNEL_NOT_FOUND"
	ErrCodeTimeout          = "TIMEOUT"
	ErrCodeConnectionFailed = "CONNECTION_FAILED"
	ErrCodeInvalidRequest   = "INVALID_REQUEST"
	ErrCodeAuthFailed       = "AUTH_FAILED"
	ErrCodeRateLimited      = "RATE_LIMITED"
)
