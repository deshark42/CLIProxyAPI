package openai

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func rawHTTPTestRouter(t *testing.T, cfg *config.Config, upstream http.Handler) http.Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(executor.NewCodexAutoExecutor(cfg))
	id := "raw-http-" + t.Name()
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: id, Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "upstream-secret", "base_url": server.URL, "header:User-Agent": "must-not-be-injected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "raw-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	router.POST("/v1/responses/compact", h.Compact)
	return router
}

func TestRawHTTPPassthroughPreservesBodiesAndHeaders(t *testing.T) {
	input := []byte("{\r\n  \"model\": \"raw-model\", \"stream\": false, \"store\": true,\r\n  \"input\": [{\"role\":\"system\",\"content\":\"你好\"}],\r\n  \"temperature\":0.75, \"max_output_tokens\":17, \"previous_response_id\":\"keep\", \"tools\":[], \"unknown\": [1,2]\r\n} \n")
	// Keep unusual JSON whitespace and a trailing newline in both directions.
	for _, tc := range []struct {
		name, path, contentType, response string
		status                            int
	}{
		{"json", "/v1/responses", "application/json", " { \"id\": \"resp_x\", \"unknown\": true }\n", 201},
		{"sse", "/v1/responses", "text/event-stream", ":heartbeat\r\nevent: codex.private\r\ndata: { \"type\": \"codex.private\" }\r\n\r\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\ndata: after-terminal\n\n", 200},
		{"compact", "/v1/responses/compact", "application/json", "{\"output\":[]}\n", 200},
		{"bad-request", "/v1/responses", "application/problem+json", " { \"error\": \"original upstream error\" } \n", 400},
		{"rate-limit", "/v1/responses", "text/plain", "untouched rate limit\r\n", 429},
		{"unauthorized", "/v1/responses", "text/plain", "upstream authentication error\n", 401},
		{"server-error", "/v1/responses", "text/plain", "original upstream failure\n", 500},
		{"redirect", "/v1/responses", "text/plain", "do not follow this redirect\n", 307},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.RawHTTPPassthrough = true
			cfg.Codex.OrphanDelegationCompatibility = true
			var calls atomic.Int32
			expectedResponse := []byte(tc.response)
			router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				got, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if !bytes.Equal(got, input) {
					t.Errorf("request changed:\nwant %q\ngot  %q", input, got)
				}
				if r.URL.Path != strings.TrimPrefix(tc.path, "/v1") || r.URL.RawQuery != "custom=a%2Fb&custom=2" {
					t.Errorf("upstream URL = %s", r.URL.String())
				}
				for name, want := range map[string]string{"Authorization": "Bearer upstream-secret", "User-Agent": "my-agent/1", "Originator": "my-agent", "X-Agent-Header": "unchanged", "Accept": "application/vnd.agent", "Content-Type": "application/json"} {
					if got := r.Header.Get(name); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				for _, name := range []string{"X-Api-Key", "X-Hop-Only", "Version", "Accept-Encoding"} {
					if got := r.Header.Get(name); got != "" {
						t.Errorf("unexpected %s: %q", name, got)
					}
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Add("X-Upstream", "one")
				w.Header().Add("X-Upstream", "two")
				w.Header().Set("Connection", "X-Response-Hop")
				w.Header().Set("X-Response-Hop", "remove")
				w.Header().Set("Location", "/not-followed")
				w.WriteHeader(tc.status)
				_, _ = w.Write(expectedResponse)
			}))
			request := httptest.NewRequest(http.MethodPost, tc.path+"?custom=a%2Fb&custom=2", bytes.NewReader(input))
			for name, value := range map[string]string{"Authorization": "Bearer proxy-secret", "X-Api-Key": "proxy-secret", "User-Agent": "my-agent/1", "Originator": "my-agent", "X-Agent-Header": "unchanged", "Accept": "application/vnd.agent", "Content-Type": "application/json", "Connection": "X-Hop-Only", "X-Hop-Only": "remove"} {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !bytes.Equal(recorder.Body.Bytes(), expectedResponse) {
				t.Errorf("response changed:\nwant %q\ngot  %q", expectedResponse, recorder.Body.Bytes())
			}
			if got := recorder.Header().Values("X-Upstream"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
				t.Errorf("upstream headers = %v", got)
			}
			if recorder.Header().Get("X-Response-Hop") != "" {
				t.Error("response hop-by-hop header leaked")
			}
			if calls.Load() != 1 {
				t.Errorf("upstream calls = %d", calls.Load())
			}
		})
	}
}

func TestRawHTTPPassthroughPreservesCompressedBodies(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	semantic := []byte(`{"model":"raw-model","stream":true,"input":"你好"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	input := encoder.EncodeAll(semantic, nil)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte("event: private\r\ndata: 你好\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	expected := append([]byte(nil), compressed.Bytes()...)
	router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, input) || r.Header.Get("Content-Encoding") != "zstd" {
			t.Error("compressed request changed")
		}
		if r.Header.Get("User-Agent") != "" {
			t.Errorf("implicit User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(expected)
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(input))
	request.Header.Set("Content-Encoding", "zstd")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), expected) || recorder.Header().Get("Content-Encoding") != "gzip" {
		t.Error("compressed response changed")
	}
}

func TestRawHTTPPassthroughRejectsMutatingPayloadRules(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"instructions": "injected"}}}
	var calls atomic.Int32
	router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"raw-model","input":"hello"}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "conflicts with payload rules") {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("modified payload reached upstream")
	}
}

func TestRawHTTPPassthroughFlushesWithoutSSEParsing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	release := make(chan struct{})
	first := []byte(": partial event without newline")
	router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "X-Upstream-Trailer")
		_, _ = w.Write(first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("\r\n\r\nlast"))
		w.Header().Set("X-Upstream-Trailer", "finished")
	}))
	server := httptest.NewServer(router)
	defer server.Close()
	// Always release the upstream before test cleanup, including assertion failures.
	var released bool
	defer func() {
		if !released {
			close(release)
		}
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"raw-model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	got := make([]byte, len(first))
	if _, err := io.ReadFull(response.Body, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("first bytes = %q", got)
	}
	close(release)
	released = true
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "\r\n\r\nlast" {
		t.Errorf("remaining body = %q", rest)
	}
	if response.Trailer.Get("X-Upstream-Trailer") != "finished" {
		t.Errorf("trailer = %v", response.Trailer)
	}
}

func TestRawHTTPPassthroughPreservesOpaqueBodies(t *testing.T) {
	for _, encoding := range []string{"", "gzip", "unknown-encoding", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.RawHTTPPassthrough = true
			input := []byte{0, 255, 32, 10, 13, 7}
			router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, input) || r.Header.Get("Content-Encoding") != encoding {
					t.Error("opaque request changed")
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write(input)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(input))
			if encoding != "" {
				request.Header.Set("Content-Encoding", encoding)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnprocessableEntity || !bytes.Equal(recorder.Body.Bytes(), input) {
				t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.Bytes())
			}
		})
	}
}

func TestRawHTTPPassthroughDisabledKeepsCompatibility(t *testing.T) {
	cfg := &config.Config{}
	router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("temperature")) {
			t.Error("normal compatibility conversion was bypassed")
		}
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			t.Errorf("upstream did not use normal streaming body: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_normal\",\"object\":\"response\",\"output\":[]}}\n\n"))
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"raw-model","input":"hi","temperature":0.7}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "resp_normal") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "data:") {
		t.Error("non-streaming compatibility response was not converted")
	}
}

func TestRawHTTPPassthroughDoesNotAppendErrorsToTruncatedBodies(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte("partial upstream bytes"))
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"raw-model","stream":true}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 200 || recorder.Body.String() != "partial upstream bytes" {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.Bytes())
	}
	if recorder.Header().Get("Content-Length") != "4096" {
		t.Error("upstream Content-Length was replaced")
	}
}
