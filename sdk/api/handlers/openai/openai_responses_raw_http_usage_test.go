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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type rawHTTPUsagePlugin func(context.Context, usage.Record)

func (f rawHTTPUsagePlugin) HandleUsage(ctx context.Context, record usage.Record) { f(ctx, record) }

func rawHTTPUsageRecords(t *testing.T) chan usage.Record {
	t.Helper()
	records := make(chan usage.Record, 16)
	id := "raw-http-" + t.Name()
	usage.RegisterNamedPlugin(t.Name(), rawHTTPUsagePlugin(func(_ context.Context, record usage.Record) {
		if record.AuthID == id {
			records <- record
		}
	}))
	usage.StartDefault(context.Background())
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), rawHTTPUsagePlugin(func(context.Context, usage.Record) {}))
	})
	return records
}

func nextRawHTTPUsageRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(5 * time.Second):
		t.Fatal("raw response usage was not published")
	}
	return usage.Record{}
}

func TestRawHTTPPassthroughPublishesUsageAndLogsWithoutRewriting(t *testing.T) {
	usageJSON := "{\"input_tokens\":100,\"output_tokens\":12,\"total_tokens\":112,\"input_tokens_details\":{\"cached_tokens\":80},\"output_tokens_details\":{\"reasoning_tokens\":4}}"
	jsonBody := " \r\n{\"id\":\"resp_usage\",\"model\":\"raw-model\",\"service_tier\":\"default\",\"usage\":" + usageJSON + "} \n"
	sse := ":heartbeat\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"id\":\"resp_usage\",\"model\":\"raw-model\",\"service_tier\":\"default\",\"usage\":" + usageJSON + "}}\r\n\r\nevent: private\ndata: do not remove me\n\n"
	for _, tc := range []struct {
		name, path, contentType, encoding, body string
		status                                  int
		failed, stream, truncated               bool
		input, cached                           int64
	}{
		{name: "json", body: jsonBody, input: 100, cached: 80},
		{name: "zero-cache", body: strings.ReplaceAll(jsonBody, `"cached_tokens":80`, `"cached_tokens":0`), input: 100},
		{name: "missing-cache-field", body: strings.ReplaceAll(jsonBody, `"input_tokens_details":{"cached_tokens":80},`, ""), input: 100},
		{name: "compact", path: "/v1/responses/compact", body: jsonBody, input: 100, cached: 80},
		{name: "sse", contentType: "text/event-stream", body: sse, stream: true, input: 100, cached: 80},
		{name: "duplicate-terminal", contentType: "text/event-stream", body: sse + sse, stream: true, input: 100, cached: 80},
		{name: "gzip-json", encoding: "gzip", body: jsonBody, input: 100, cached: 80},
		{name: "gzip-sse", encoding: "gzip", contentType: "text/event-stream", body: sse, stream: true, input: 100, cached: 80},
		{name: "zstd-sse", encoding: "zstd", contentType: "text/event-stream", body: sse, stream: true, input: 100, cached: 80},
		{name: "no-usage", body: "{\"id\":\"resp_without_usage\"}"},
		{name: "http-error", status: 429, body: "{\"error\":{\"message\":\"original limit error\"}}", failed: true},
		{name: "sse-error", contentType: "text/event-stream", body: strings.ReplaceAll(sse, "response.completed", "response.failed"), stream: true, input: 100, cached: 80, failed: true},
		{name: "truncated", body: jsonBody, input: 100, cached: 80, truncated: true, failed: true},
		{name: "opaque-encoding", encoding: "opaque", body: jsonBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := rawHTTPUsageRecords(t)
			cfg := &config.Config{}
			cfg.Codex.RawHTTPPassthrough = true
			cfg.RequestLog = true
			path, contentType, status := tc.path, tc.contentType, tc.status
			if path == "" {
				path = "/v1/responses"
			}
			if contentType == "" {
				contentType = "application/json"
			}
			if status == 0 {
				status = http.StatusOK
			}
			expected := []byte(tc.body)
			var compressed bytes.Buffer
			switch tc.encoding {
			case "gzip":
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(expected); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				expected = compressed.Bytes()
			case "zstd":
				writer, err := zstd.NewWriter(&compressed)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(expected); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				expected = compressed.Bytes()
			}
			requestBody := " {\n \"model\":\"raw-model\", \"stream\":false, \"prompt_cache_key\":\"client-cache-key\", \"unknown\":true\n} "
			var requestLog, responseLog string
			router := rawHTTPTestRouter(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if string(got) != requestBody {
					t.Errorf("request bytes changed: %q", got)
				}
				if r.Header.Get("Session-Id") != "client-session" {
					t.Errorf("session header changed: %v", r.Header)
				}
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("X-Raw-Response", "keep")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				if tc.truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(expected)+128))
				}
				w.WriteHeader(status)
				if _, err := w.Write(expected); err != nil {
					t.Error(err)
				}
			}), func(c *gin.Context) {
				c.Next()
				if value, ok := c.Get("API_REQUEST"); ok {
					requestLog = string(value.([]byte))
				}
				if value, ok := c.Get("API_RESPONSE"); ok {
					responseLog = string(value.([]byte))
				}
			})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Session-Id", "client-session")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != status || !bytes.Equal(recorder.Body.Bytes(), expected) {
				t.Fatalf("response changed: status %d, body %q", recorder.Code, recorder.Body.Bytes())
			}
			if recorder.Header().Get("Content-Encoding") != tc.encoding || recorder.Header().Get("X-Raw-Response") != "keep" {
				t.Fatalf("response headers changed: %v", recorder.Header())
			}
			if !strings.Contains(requestLog, requestBody) || !strings.Contains(requestLog, "/responses") {
				t.Fatalf("upstream request log missing: %q", requestLog)
			}
			if !strings.Contains(responseLog, "Status: "+strconv.Itoa(status)) || !strings.Contains(responseLog, "keep") {
				t.Fatalf("upstream response log missing: %q", responseLog)
			}
			if tc.input > 0 && tc.encoding == "" && !strings.Contains(responseLog, "input_tokens") {
				t.Fatalf("upstream response body log missing: %q", responseLog)
			}
			if strings.Contains(requestLog, "upstream-secret") {
				t.Fatal("upstream credential was not redacted in the request log")
			}
			record := nextRawHTTPUsageRecord(t, records)
			if record.Provider != "codex" || record.Model != "raw-model" || record.Failed != tc.failed || record.Stream != tc.stream {
				t.Fatalf("usage record metadata = %+v", record)
			}
			if record.Detail.InputTokens != tc.input || record.Detail.CachedTokens != tc.cached || record.Detail.CacheReadTokens != tc.cached {
				t.Fatalf("usage record detail = %+v", record.Detail)
			}
			if tc.input > 0 && (record.Detail.OutputTokens != 12 || record.Detail.TotalTokens != 112 || record.Detail.ReasoningTokens != 4 || record.ResponseModel != "raw-model" || record.ResponseServiceTier != "default") {
				t.Fatalf("usage record = %+v", record)
			}
			if tc.name == "http-error" && record.Fail.StatusCode != status {
				t.Fatalf("failure = %+v", record.Fail)
			}
			if record.ResponseHeaders.Get("X-Raw-Response") != "keep" {
				t.Fatalf("usage response headers = %v", record.ResponseHeaders)
			}
			// A queue barrier makes the exactly-once assertion independent of dispatcher timing.
			usage.PublishRecord(context.Background(), usage.Record{AuthID: "raw-http-" + t.Name(), Model: "raw-usage-barrier"})
			if next := nextRawHTTPUsageRecord(t, records); next.Model != "raw-usage-barrier" {
				t.Fatalf("duplicate usage publication: %+v", next)
			}
		})
	}
}
