package httputil

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CopyHeaders copies all headers from src to dst.
func CopyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// CleanHopByHopHeaders removes hop-by-hop headers that should not be forwarded.
func CleanHopByHopHeaders(headers http.Header) {
	if headers == nil {
		return
	}

	if connectionHeaders := headers.Get("Connection"); connectionHeaders != "" {
		for _, token := range strings.Split(connectionHeaders, ",") {
			if t := strings.TrimSpace(token); t != "" {
				headers.Del(http.CanonicalHeaderKey(t))
			}
		}
	}

	for _, key := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Proxy-Connection",
	} {
		headers.Del(key)
	}
}

// WriteProxyError writes an HTTP error response to the writer.
func WriteProxyError(w io.Writer, code int, msg string) {
	body := msg
	resp := &http.Response{
		StatusCode:    code,
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	_ = resp.Write(w)
	_ = resp.Body.Close()
}

// IsWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func IsWebSocketUpgrade(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}
