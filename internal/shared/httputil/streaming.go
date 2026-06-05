package httputil

import (
	"mime"
	"net/http"
	"strings"
)

const eventStreamMediaType = "text/event-stream"

// IsEventStream reports whether headers describe a Server-Sent Events response.
func IsEventStream(headers http.Header) bool {
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}

	return strings.EqualFold(mediaType, eventStreamMediaType)
}
