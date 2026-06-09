package httputil

import (
	"net/http"
	"testing"
)

func TestIsEventStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{
			name:        "plain event stream",
			contentType: "text/event-stream",
			want:        true,
		},
		{
			name:        "event stream with charset",
			contentType: "text/event-stream; charset=utf-8",
			want:        true,
		},
		{
			name:        "event stream with mixed case",
			contentType: "Text/Event-Stream; Charset=UTF-8",
			want:        true,
		},
		{
			name:        "json is not event stream",
			contentType: "application/json",
			want:        false,
		},
		{
			name:        "empty content type",
			contentType: "",
			want:        false,
		},
		{
			name:        "invalid parameter still matches media type",
			contentType: "text/event-stream; charset",
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			if tt.contentType != "" {
				header.Set("Content-Type", tt.contentType)
			}

			if got := IsEventStream(header); got != tt.want {
				t.Fatalf("IsEventStream(%q) = %v, want %v", tt.contentType, got, tt.want)
			}
		})
	}
}
