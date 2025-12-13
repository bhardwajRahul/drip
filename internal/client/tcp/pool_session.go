package tcp

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"

	"drip/internal/shared/constants"
	"drip/internal/shared/protocol"
)

// sessionHandle wraps a yamux session with metadata.
type sessionHandle struct {
	id         string
	conn       net.Conn
	session    *yamux.Session
	active     atomic.Int64
	lastActive atomic.Int64 // unix nanos
	closed     atomic.Bool
}

func (h *sessionHandle) touch() {
	h.lastActive.Store(time.Now().UnixNano())
}

func (h *sessionHandle) lastActiveTime() time.Time {
	n := h.lastActive.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// scalerLoop monitors load and adjusts session count.
func (c *PoolClient) scalerLoop() {
	defer c.wg.Done()

	const (
		checkInterval      = 5 * time.Second
		scaleUpCooldown    = 5 * time.Second
		scaleDownCooldown  = 60 * time.Second
		capacityPerSession = int64(64)
		scaleUpLoad        = 0.7
		scaleDownLoad      = 0.3
	)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}

		c.mu.Lock()
		desired := c.desiredTotal
		if desired == 0 {
			desired = c.initialSessions
			c.desiredTotal = desired
		}
		lastScale := c.lastScale
		c.mu.Unlock()

		current := c.sessionCount()
		if current <= 0 {
			continue
		}

		active := c.stats.GetActiveConnections()
		load := float64(active) / float64(int64(current)*capacityPerSession)

		sinceLastScale := time.Since(lastScale)
		if sinceLastScale >= scaleUpCooldown && load > scaleUpLoad && desired < c.maxSessions {
			c.mu.Lock()
			c.desiredTotal = min(c.desiredTotal+1, c.maxSessions)
			c.lastScale = time.Now()
			c.mu.Unlock()
		} else if sinceLastScale >= scaleDownCooldown && load < scaleDownLoad && desired > c.minSessions {
			c.mu.Lock()
			c.desiredTotal = max(c.desiredTotal-1, c.minSessions)
			c.lastScale = time.Now()
			c.mu.Unlock()
		}

		c.ensureSessions()
	}
}

// ensureSessions adjusts session count to match desired.
func (c *PoolClient) ensureSessions() {
	if c.IsClosed() || c.tunnelID == "" {
		return
	}

	c.mu.RLock()
	desired := c.desiredTotal
	c.mu.RUnlock()

	desired = min(max(desired, c.minSessions), c.maxSessions)

	current := c.sessionCount()
	if current < desired {
		for i := 0; i < desired-current; i++ {
			if err := c.addDataSession(); err != nil {
				c.logger.Debug("Add data session failed", zap.Error(err))
				break
			}
		}
		return
	}

	if current > desired {
		c.removeIdleSessions(current - desired)
	}
}

// addDataSession creates a new data session.
func (c *PoolClient) addDataSession() error {
	select {
	case <-c.stopCh:
		return net.ErrClosed
	default:
	}

	if c.tunnelID == "" {
		return fmt.Errorf("server does not support data connections")
	}

	conn, err := c.dialTLS()
	if err != nil {
		return err
	}

	connID := fmt.Sprintf("data-%d", time.Now().UnixNano())

	req := protocol.DataConnectRequest{
		TunnelID:     c.tunnelID,
		Token:        c.token,
		ConnectionID: connID,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to marshal data connect request: %w", err)
	}

	if err := protocol.WriteFrame(conn, protocol.NewFrame(protocol.FrameTypeDataConnect, payload)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to send data connect: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	ack, err := protocol.ReadFrame(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to read data connect ack: %w", err)
	}
	defer ack.Release()
	_ = conn.SetReadDeadline(time.Time{})

	if ack.Type == protocol.FrameTypeError {
		var errMsg protocol.ErrorMessage
		if e := json.Unmarshal(ack.Payload, &errMsg); e == nil {
			_ = conn.Close()
			return fmt.Errorf("data connect error: %s - %s", errMsg.Code, errMsg.Message)
		}
		_ = conn.Close()
		return fmt.Errorf("data connect error")
	}
	if ack.Type != protocol.FrameTypeDataConnectAck {
		_ = conn.Close()
		return fmt.Errorf("unexpected data connect ack frame: %s", ack.Type)
	}

	var resp protocol.DataConnectResponse
	if err := json.Unmarshal(ack.Payload, &resp); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to parse data connect response: %w", err)
	}
	if !resp.Accepted {
		_ = conn.Close()
		return fmt.Errorf("data connection rejected: %s", resp.Message)
	}

	yamuxCfg := yamux.DefaultConfig()
	yamuxCfg.EnableKeepAlive = false
	yamuxCfg.LogOutput = io.Discard
	yamuxCfg.AcceptBacklog = constants.YamuxAcceptBacklog

	session, err := yamux.Server(conn, yamuxCfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to init yamux session: %w", err)
	}

	h := &sessionHandle{
		id:      connID,
		conn:    conn,
		session: session,
	}
	h.touch()

	c.mu.Lock()
	c.dataSessions[connID] = h
	c.mu.Unlock()

	c.wg.Add(1)
	go c.acceptLoop(h, false)

	c.wg.Add(1)
	go c.sessionWatcher(h, false)

	return nil
}

// removeIdleSessions removes n idle sessions.
func (c *PoolClient) removeIdleSessions(n int) {
	if n <= 0 {
		return
	}

	type candidate struct {
		id         string
		active     int64
		lastActive time.Time
	}

	c.mu.RLock()
	candidates := make([]candidate, 0, len(c.dataSessions))
	for id, h := range c.dataSessions {
		candidates = append(candidates, candidate{
			id:         id,
			active:     h.active.Load(),
			lastActive: h.lastActiveTime(),
		})
	}
	c.mu.RUnlock()

	removed := 0
	for removed < n {
		var best candidate
		found := false
		for _, cand := range candidates {
			if cand.active != 0 {
				continue
			}
			if !found || cand.lastActive.Before(best.lastActive) {
				best = cand
				found = true
			}
		}
		if !found {
			return
		}
		if c.removeDataSession(best.id) {
			removed++
		}
		for i := range candidates {
			if candidates[i].id == best.id {
				candidates[i].active = 1
				break
			}
		}
	}
}

// removeDataSession removes a data session by ID.
func (c *PoolClient) removeDataSession(id string) bool {
	var h *sessionHandle

	c.mu.Lock()
	h = c.dataSessions[id]
	delete(c.dataSessions, id)
	c.mu.Unlock()

	if h == nil {
		return false
	}

	if !h.closed.CompareAndSwap(false, true) {
		return false
	}

	if h.session != nil {
		_ = h.session.Close()
	}
	if h.conn != nil {
		_ = h.conn.Close()
	}

	return true
}

// sessionCount returns the total number of active sessions.
func (c *PoolClient) sessionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := len(c.dataSessions)
	if c.primary != nil {
		count++
	}
	return count
}
