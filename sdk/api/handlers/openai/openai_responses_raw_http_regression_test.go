package openai

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func rawHTTPTestServe(t *testing.T, router http.Handler, writer http.ResponseWriter, request *http.Request, wantAbort bool) {
	t.Helper()
	defer func() {
		recovered := recover()
		if wantAbort && recovered != http.ErrAbortHandler {
			t.Errorf("abort signal = %v, want http.ErrAbortHandler", recovered)
		} else if !wantAbort && recovered != nil {
			panic(recovered)
		}
	}()
	router.ServeHTTP(writer, request)
}

func TestRawHTTPPassthroughStripsProxyCredentials(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.RawHTTPPassthrough = true
			const query = "custom=a%2fb&key=secret&auth_token=secret&custom=2&%6bey=other&auth%5Ftoken=other&key&auth_token=&plus=a+b&space=a%20b"
			var calls atomic.Int32
			router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if got := r.URL.RawQuery; got != "custom=a%2fb&custom=2&plus=a+b&space=a%20b" {
					t.Errorf("upstream query = %q", got)
				}
				for _, name := range []string{"X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization"} {
					if got := r.Header.Get(name); got != "" {
						t.Errorf("upstream %s = %q", name, got)
					}
				}
				if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
					t.Errorf("upstream Authorization = %q", got)
				}
				_, _ = io.WriteString(w, "{}")
			}))
			request := httptest.NewRequest(http.MethodPost, path+"?"+query, strings.NewReader(`{"model":"raw-model"}`))
			for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization"} {
				request.Header.Set(name, "secret")
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status = %d, calls = %d", recorder.Code, calls.Load())
			}
			if request.URL.RawQuery != query || request.Header.Get("X-Goog-Api-Key") != "secret" {
				t.Fatal("incoming credentials were mutated")
			}
		})
	}
}

func TestRawHTTPPassthroughRejectsUnknownModel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.RawHTTPPassthrough = true
	var calls atomic.Int32
	router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"unregistered-codex-model"}`)))
		if recorder.Code < 400 || calls.Load() != 0 {
			t.Fatalf("unknown model: status = %d, upstream calls = %d", recorder.Code, calls.Load())
		}
	}
}

func TestRawHTTPPassthroughRecordsBodyOutcomeOnce(t *testing.T) {
	for _, event := range []string{"response.completed", "response.failed", "error"} {
		for _, encoding := range []string{"identity", "gzip"} {
			t.Run(event+"/"+encoding, func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Codex.RawHTTPPassthrough = true
				body := "data: {\"type\":\"" + event + "\"}\n\n"
				wire := []byte(body + body)
				if encoding == "gzip" {
					var buffer bytes.Buffer
					writer := gzip.NewWriter(&buffer)
					if _, err := writer.Write(wire); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					wire = buffer.Bytes()
				}
				_, manager, id := rawHTTPTestRouterWithManager(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Encoding", encoding)
					_, _ = w.Write(wire)
				}))
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"raw-model","stream":true}`))
				response, err := manager.RoundTripCodexResponses(context.Background(), request, []byte(`{"model":"raw-model","stream":true}`), coreexecutor.Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = response.Body.Close() })
				before, _ := manager.GetByID(id)
				if before.Success != 0 || before.Failed != 0 {
					t.Fatalf("result published before body consumption: %+v", before)
				}
				got, err := io.ReadAll(response.Body)
				if err != nil || !bytes.Equal(got, wire) {
					t.Fatalf("body = %q, error = %v", got, err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				auth, _ := manager.GetByID(id)
				if event == "response.completed" {
					if auth.Success != 1 || auth.Failed != 0 {
						t.Fatalf("success=%d failed=%d", auth.Success, auth.Failed)
					}
				} else if auth.Success != 0 || auth.Failed != 1 {
					t.Fatalf("success=%d failed=%d", auth.Success, auth.Failed)
				}
			})
		}
	}
}

func TestRawHTTPPassthroughHonorsRetryAfter(t *testing.T) {
	for _, value := range []string{"3600", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(value, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.RawHTTPPassthrough = true
			const body = `{"error":{"message":"rate limited"}}`
			_, manager, id := rawHTTPTestRouterWithManager(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", value)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"raw-model"}`))
			before := time.Now()
			response, err := manager.RoundTripCodexResponses(context.Background(), request, []byte(`{"model":"raw-model"}`), coreexecutor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = response.Body.Close() })
			auth, _ := manager.GetByID(id)
			if auth.Success != 0 || auth.Failed != 0 {
				t.Fatalf("result published before error body observation: %+v", auth)
			}
			got, err := io.ReadAll(response.Body)
			if err != nil || string(got) != body || response.Header.Get("Retry-After") != value || response.StatusCode != 429 {
				t.Fatalf("response changed: status=%d body=%q err=%v headers=%v", response.StatusCode, got, err, response.Header)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			auth, _ = manager.GetByID(id)
			state := auth.ModelStates["raw-model"]
			if state == nil || state.NextRetryAfter.Before(before.Add(3598*time.Second)) || state.NextRetryAfter.After(time.Now().Add(3601*time.Second)) {
				t.Fatalf("Retry-After %q: model state = %+v", value, state)
			}
			if auth.Success != 0 || auth.Failed != 1 {
				t.Fatalf("success=%d failed=%d", auth.Success, auth.Failed)
			}
		})
	}
}

func TestRawHTTPPassthroughAbortsTruncatedChunkedResponse(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "http1", true: "http2"}[http2], func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.RawHTTPPassthrough = true
			router, manager, id := rawHTTPTestRouterWithManager(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
				_ = buffer.Flush()
			}), logging.GinLogrusRecovery())
			server := httptest.NewUnstartedServer(router)
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			response, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"raw-model","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			got, err := io.ReadAll(response.Body)
			if err == nil || errors.Is(err, io.EOF) || string(got) != "hello" || response.StatusCode != 200 {
				t.Fatalf("status=%d body=%q error=%v", response.StatusCode, got, err)
			}
			if response.ProtoMajor != map[bool]int{false: 1, true: 2}[http2] {
				t.Fatalf("protocol = %s", response.Proto)
			}
			auth, _ := manager.GetByID(id)
			if auth.Success != 0 || auth.Failed != 1 {
				t.Fatalf("success=%d failed=%d", auth.Success, auth.Failed)
			}
		})
	}
}
