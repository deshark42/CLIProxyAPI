package helps

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// EnsureCodexSessionHeader mirrors the caller's session identity onto the canonical
// "Session-Id" header.
//
// The Codex backend derives its prompt cache key from that header and ignores the
// prompt_cache_key field of the request body. A deployment whose ingress drops headers
// containing underscores therefore loses prefix caching completely: the backend mints a
// fresh key for every request and reports zero cached tokens, even though the body asks
// for a stable cache key. Re-emitting the value under the canonical name keeps the body
// byte-identical while restoring cache affinity.
func EnsureCodexSessionHeader(headers http.Header, payload []byte) {
	if headers == nil {
		return
	}
	if strings.TrimSpace(headerValueCaseInsensitive(headers, "Session-Id")) != "" {
		return
	}
	sessionID := ""
	for _, name := range []string{"Session_id", "session_id", "X-Session-Id"} {
		if sessionID = strings.TrimSpace(headerValueCaseInsensitive(headers, name)); sessionID != "" {
			break
		}
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String())
	}
	if sessionID == "" {
		return
	}
	headers.Set("Session-Id", sessionID)
}
