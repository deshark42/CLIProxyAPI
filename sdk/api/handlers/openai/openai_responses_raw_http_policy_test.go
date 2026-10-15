package openai

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func rawHTTPPolicyEncode(t *testing.T, body []byte, encoding string) []byte {
	t.Helper()
	if encoding == "" {
		return body
	}
	var buffer bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&buffer)
	case "zstd":
		encoder, err := zstd.NewWriter(&buffer)
		if err != nil {
			t.Fatal(err)
		}
		writer = encoder
	default:
		t.Fatalf("unexpected encoding %q", encoding)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRawHTTPPassthroughFailsClosedForUninspectablePayloadRules(t *testing.T) {
	rules := []struct {
		name      string
		configure func(*config.Config)
	}{
		{"default", func(c *config.Config) {
			c.Payload.Default = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"instructions": "required"}}}
		}},
		{"default-raw", func(c *config.Config) {
			c.Payload.DefaultRaw = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"instructions": `"required"`}}}
		}},
		{"override", func(c *config.Config) {
			c.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"instructions": "required"}}}
		}},
		{"override-raw", func(c *config.Config) {
			c.Payload.OverrideRaw = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"instructions": `"required"`}}}
		}},
		{"filter", func(c *config.Config) {
			c.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"tools"}}}
		}},
		{"disable-image-generation", func(c *config.Config) { c.DisableImageGeneration = config.DisableImageGenerationAll }},
	}
	plain := []byte(`{"model":"raw-model","tools":[{"type":"image_generation"}]}`)
	bodies := []struct {
		name, encoding string
		body           []byte
	}{
		{"json", "", plain},
		{"gzip", "gzip", rawHTTPPolicyEncode(t, plain, "gzip")},
		{"zstd", "zstd", rawHTTPPolicyEncode(t, plain, "zstd")},
		{"unknown-encoding", "unknown-encoding", []byte{0, 255}},
		{"malformed-zstd", "zstd", []byte{0, 255}},
		{"invalid-json-with-model", "", []byte(`{"model":"raw-model",`)},
		{"missing-model", "", []byte(`{"tools":[]}`)},
	}
	for _, rule := range rules {
		for _, body := range bodies {
			for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
				t.Run(rule.name+"/"+body.name+path, func(t *testing.T) {
					cfg := &config.Config{}
					cfg.Codex.RawHTTPPassthrough = true
					rule.configure(cfg)
					var calls atomic.Int32
					router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
					request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body.body))
					request.Header.Set("Content-Encoding", body.encoding)
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)
					if recorder.Code != http.StatusBadRequest || calls.Load() != 0 {
						t.Fatalf("status=%d upstream calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body.String())
					}
				})
			}
		}
	}
}

func TestRawHTTPPassthroughAllowsUnchangedPayloadRules(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"tools"}}}
	plain := []byte(` {"model":"raw-model", "input":"unchanged"} `)
	for _, encoding := range []string{"", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			wire := rawHTTPPolicyEncode(t, plain, encoding)
			router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(got, wire) {
					t.Errorf("request changed: body=%q err=%v", got, err)
				}
				_, _ = io.WriteString(w, "{}")
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
			request.Header.Set("Content-Encoding", encoding)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRawHTTPPassthroughPreservesErrorCoolingSemantics(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Unix()
	quota := `{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":3600}`
	cases := []struct {
		name, body, contentType string
		status                  int
		quota                   bool
	}{
		{"previous-response-not-found", `{"error":{"code":"previous_response_not_found","type":"invalid_request_error","message":"Previous response not found"}}`, "application/json", 404, false},
		{"http-quota-relative-reset", `{"error":` + quota + `}`, "application/json", 429, true},
		{"http-quota-absolute-reset", `{"error":{"type":"usage_limit_reached","resets_at":` + strconv.FormatInt(resetAt, 10) + `}}`, "application/json", 429, true},
		{"sse-request-error", "data: " + `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"context too long"}}}` + "\n\n", "text/event-stream", 200, false},
		{"sse-error-quota", "data: " + `{"type":"error","error":` + quota + `}` + "\n\n", "text/event-stream", 200, true},
		{"sse-response-failed-quota", "data: " + `{"type":"response.failed","response":{"error":` + quota + `}}` + "\n\n", "text/event-stream", 200, true},
	}
	for _, tc := range cases {
		for _, encoding := range []string{"", "gzip", "zstd"} {
			for _, modelLevelCooling := range []bool{false, true} {
				t.Run(tc.name+"/"+encoding+"/model-cooling="+strconv.FormatBool(modelLevelCooling), func(t *testing.T) {
					cfg := &config.Config{}
					cfg.Codex.RawHTTPPassthrough = true
					cfg.Codex.ModelLevelCooling = modelLevelCooling
					wire := rawHTTPPolicyEncode(t, []byte(tc.body), encoding)
					_, manager, id := rawHTTPTestRouterWithManager(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", tc.contentType)
						w.Header().Set("Content-Encoding", encoding)
						w.Header().Set("Trailer", "X-Raw-Error-Trailer")
						w.WriteHeader(tc.status)
						_, _ = w.Write(wire)
						w.Header().Set("X-Raw-Error-Trailer", "unchanged")
					}))
					payload := []byte(`{"model":"raw-model","stream":true}`)
					request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
					before := time.Now()
					response, err := manager.RoundTripCodexResponses(context.Background(), request, payload, coreexecutor.Options{})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = response.Body.Close() })
					auth, _ := manager.GetByID(id)
					if auth.Success != 0 || auth.Failed != 0 {
						t.Fatalf("result published before body observation: %+v", auth)
					}
					got, err := io.ReadAll(response.Body)
					if err != nil || !bytes.Equal(got, wire) || response.StatusCode != tc.status || response.Header.Get("Content-Encoding") != encoding || response.Trailer.Get("X-Raw-Error-Trailer") != "unchanged" {
						t.Fatalf("response changed: status=%d body=%q err=%v headers=%v trailers=%v", response.StatusCode, got, err, response.Header, response.Trailer)
					}
					for i := 0; i < 2; i++ {
						if err := response.Body.Close(); err != nil {
							t.Fatal(err)
						}
					}
					auth, _ = manager.GetByID(id)
					if auth.Success != 0 || auth.Failed != 1 {
						t.Fatalf("success=%d failed=%d", auth.Success, auth.Failed)
					}
					state := auth.ModelStates["raw-model"]
					if tc.quota {
						if state == nil || !state.Quota.Exceeded || state.NextRetryAfter.Before(before.Add(3590*time.Second)) || state.NextRetryAfter.After(time.Now().Add(3605*time.Second)) {
							t.Fatalf("quota reset lost: model=%+v", state)
						}
						if (auth.Quota.Reason == "credential_quota") == modelLevelCooling {
							t.Fatalf("quota scope changed: model-cooling=%t quota=%+v", modelLevelCooling, auth.Quota)
						}
					} else if state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
						t.Fatalf("caller error cooled healthy credential: %+v", state)
					}
					if !tc.quota && auth.Quota.Exceeded {
						t.Fatalf("caller error marked quota exceeded: %+v", auth.Quota)
					}
				})
			}
		}
	}
}

func TestRawHTTPPassthroughHTTPErrorEarlyCloseRetainsStatus(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	_, manager, id := rawHTTPTestRouterWithManager(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "opaque error body")
	}))
	payload := []byte(`{"model":"raw-model"}`)
	response, err := manager.RoundTripCodexResponses(context.Background(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(payload))), payload, coreexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	auth, _ := manager.GetByID(id)
	state := auth.ModelStates["raw-model"]
	if auth.Failed != 1 || state == nil || state.LastError == nil || state.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("HTTP failure lost after early close: failed=%d state=%+v", auth.Failed, state)
	}
}
