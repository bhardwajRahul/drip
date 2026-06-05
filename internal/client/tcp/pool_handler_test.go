package tcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"drip/internal/shared/protocol"
	"drip/internal/shared/stats"

	"go.uber.org/zap"
)

type recordingConn struct {
	net.Conn
	mu            sync.Mutex
	writeDeadline time.Time
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *recordingConn) lastWriteDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeDeadline
}

func TestHandleHTTPStreamForwardsEventStreamWithoutShortWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Errorf("path = %q, want /events", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("backend ResponseWriter does not support flush")
			return
		}

		_, _ = fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		<-release
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		backend.Close()
	}()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	localHost, localPortText, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("split backend host: %v", err)
	}
	localPort, err := strconv.Atoi(localPortText)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	poolClient := &PoolClient{
		tunnelType: protocol.TunnelTypeHTTP,
		localHost:  localHost,
		localPort:  localPort,
		httpClient: newLocalHTTPClient(protocol.TunnelTypeHTTP, false),
		ctx:        context.Background(),
		stats:      stats.NewTrafficStats(),
		logger:     zap.NewNop(),
	}

	serverSide, rawClientSide := net.Pipe()
	defer serverSide.Close()
	clientSide := &recordingConn{Conn: rawClientSide}

	done := make(chan struct{})
	go func() {
		defer close(done)
		poolClient.handleHTTPStream(clientSide)
	}()

	req, err := http.NewRequest(http.MethodGet, "http://demo.tunnels.test/events", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := req.Write(serverSide); err != nil {
		t.Fatalf("write request to stream: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(serverSide), req)
	if err != nil {
		t.Fatalf("read response from stream: %v", err)
	}
	defer resp.Body.Close()

	got := readClientBodyWithin(t, resp.Body, len("data: first\n\n"), time.Second)
	if got != "data: first\n\n" {
		t.Fatalf("body prefix = %q, want first SSE event", got)
	}

	if deadline := clientSide.lastWriteDeadline(); !deadline.IsZero() {
		t.Fatalf("SSE stream write deadline = %v, want zero", deadline)
	}

	releaseOnce.Do(func() { close(release) })
	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handleHTTPStream did not return after stream close")
	}
}

func readClientBodyWithin(t *testing.T, body io.Reader, size int, timeout time.Duration) string {
	t.Helper()

	type readResult struct {
		data []byte
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, size)
		_, err := io.ReadFull(body, buf)
		resultCh <- readResult{data: buf, err: err}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("read response body: %v", result.err)
		}
		return string(result.data)
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for response body")
	}

	return ""
}
