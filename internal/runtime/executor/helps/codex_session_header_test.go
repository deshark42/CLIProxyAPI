package helps

import (
	"net/http"
	"testing"
)

func TestEnsureCodexSessionHeader(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","prompt_cache_key":"body-cache-key"}`)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"canonical present is kept", http.Header{"Session-Id": {"canonical-key"}}, "canonical-key"},
		{"underscore mirrored", http.Header{"Session_id": {"session-abc"}}, "session-abc"},
		{"lowercase underscore mirrored", http.Header{"session_id": {"session-lower"}}, "session-lower"},
		{"x-session mirrored", http.Header{"X-Session-Id": {"session-x"}}, "session-x"},
		{"body cache key used as fallback", http.Header{}, "body-cache-key"},
		{"canonical wins over underscore", http.Header{"Session-Id": {"canonical-key"}, "Session_id": {"session-abc"}}, "canonical-key"},
		{"nothing to mirror", http.Header{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := body
			if tc.name == "nothing to mirror" {
				payload = []byte(`{"model":"gpt-6.1-sol"}`)
			}
			headers := tc.headers.Clone()
			EnsureCodexSessionHeader(headers, payload)
			if got := headers.Get("Session-Id"); got != tc.want {
				t.Fatalf("Session-Id = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnsureCodexSessionHeaderKeepsOriginalHeaders(t *testing.T) {
	headers := http.Header{"Session_id": {"session-abc"}, "X-Agent-Header": {"unchanged"}}
	payload := []byte(`{"prompt_cache_key":"body-cache-key"}`)
	before := string(payload)
	EnsureCodexSessionHeader(headers, payload)
	if string(payload) != before {
		t.Fatalf("payload changed: %s", payload)
	}
	if got := headers.Get("Session_id"); got != "session-abc" {
		t.Fatalf("Session_id = %q, want original value preserved", got)
	}
	if got := headers.Get("X-Agent-Header"); got != "unchanged" {
		t.Fatalf("X-Agent-Header = %q, want unchanged", got)
	}
	if got := headers.Get("Session-Id"); got != "session-abc" {
		t.Fatalf("Session-Id = %q, want %q", got, "session-abc")
	}
}

func TestEnsureCodexSessionHeaderNilHeader(t *testing.T) {
	EnsureCodexSessionHeader(nil, []byte(`{"prompt_cache_key":"body-cache-key"}`))
}
