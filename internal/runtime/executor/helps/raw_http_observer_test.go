package helps

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func rawHTTPEncodeTestBody(t *testing.T, body []byte, encoding string) []byte {
	t.Helper()
	for _, coding := range strings.Split(encoding, ",") {
		var buffer bytes.Buffer
		var writer io.WriteCloser
		switch strings.TrimSpace(coding) {
		case "", "identity":
			continue
		case "gzip":
			writer = gzip.NewWriter(&buffer)
		case "deflate":
			writer = zlib.NewWriter(&buffer)
		case "br":
			writer = brotli.NewWriter(&buffer)
		case "zstd":
			encoder, err := zstd.NewWriter(&buffer)
			if err != nil {
				t.Fatal(err)
			}
			writer = encoder
		default:
			t.Fatalf("unexpected encoding %q", coding)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body = append([]byte(nil), buffer.Bytes()...)
	}
	return body
}

type rawHTTPTestChunkReader struct{ io.Reader }

func (r rawHTTPTestChunkReader) Read(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return r.Reader.Read(p)
}

func TestRawHTTPObserverPreservesEncodedBytesAndExtractsUsage(t *testing.T) {
	jsonBody := " {\"model\":\"served-model\",\"usage\":{\"input_tokens\":100,\"output_tokens\":12,\"total_tokens\":112,\"input_tokens_details\":{\"cached_tokens\":80},\"output_tokens_details\":{\"reasoning_tokens\":4}}} \n"
	sseBody := ":keepalive\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"model\":\"served-model\",\"usage\":{\"input_tokens\":100,\"output_tokens\":12,\"total_tokens\":112,\"input_tokens_details\":{\"cached_tokens\":80},\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\r\n\r\nevent: private\ndata: untouched after completion\n\n"
	for _, sse := range []bool{false, true} {
		for _, encoding := range []string{"identity", "gzip", "zstd", "br", "deflate", "gzip, br"} {
			t.Run(encoding+map[bool]string{false: "-json", true: "-sse"}[sse], func(t *testing.T) {
				payload, contentType := jsonBody, "application/json"
				if sse {
					payload, contentType = sseBody, "text/event-stream"
				}
				wire := rawHTTPEncodeTestBody(t, []byte(payload), encoding)
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}, "Content-Encoding": {encoding}}, Body: io.NopCloser(rawHTTPTestChunkReader{bytes.NewReader(wire)})}
				reporter := NewUsageReporter(context.Background(), "codex", "raw-model", nil)
				observer := ObserveRawHTTPResponse(context.Background(), nil, reporter, response, nil).(*rawHTTPResponseBody)
				got, err := io.ReadAll(observer)
				if err != nil {
					t.Fatal(err)
				}
				if err := observer.Close(); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, wire) {
					t.Fatal("forwarded response bytes changed")
				}
				detail := observer.parser.detail
				if detail.InputTokens != 100 || detail.OutputTokens != 12 || detail.TotalTokens != 112 || detail.CachedTokens != 80 || detail.CacheReadTokens != 80 || detail.ReasoningTokens != 4 {
					t.Fatalf("usage = %+v", detail)
				}
				if reporter.responseModel != "served-model" {
					t.Fatalf("response model = %q", reporter.responseModel)
				}
			})
		}
	}
}

// Codex upstream responses stream SSE without any Content-Type header, so the observed
// usage must not depend on it.
func TestRawHTTPObserverDetectsBodyFormatWithoutContentType(t *testing.T) {
	usageFields := "{\"input_tokens\":100,\"output_tokens\":12,\"total_tokens\":112,\"input_tokens_details\":{\"cached_tokens\":80},\"output_tokens_details\":{\"reasoning_tokens\":4}}"
	created := "{\"type\":\"response.created\",\"response\":{\"model\":\"served-model\"}}"
	completed := "{\"type\":\"response.completed\",\"response\":{\"model\":\"served-model\",\"usage\":" + usageFields + "}}"
	sseBody := "event: response.created\ndata: " + created + "\n\n" + "event: response.completed\ndata: " + completed + "\n\n"
	jsonBody := "{\"model\":\"served-model\",\"usage\":" + usageFields + "}\n"
	cases := []struct {
		name        string
		contentType string
		encoding    string
		body        string
		wantUsage   bool
	}{
		{name: "absent content type sse", body: sseBody, wantUsage: true},
		{name: "opaque content type sse", contentType: "application/octet-stream", body: sseBody, wantUsage: true},
		{name: "absent content type gzipped sse", encoding: "gzip", body: sseBody, wantUsage: true},
		{name: "absent content type json", body: jsonBody, wantUsage: true},
		{name: "explicit json", contentType: "application/json", body: jsonBody, wantUsage: true},
		{name: "explicit event stream", contentType: "text/event-stream", body: sseBody, wantUsage: true},
		{name: "unrecognized body", body: strings.Repeat("x", rawHTTPUsageSniffLimit+16), wantUsage: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := rawHTTPEncodeTestBody(t, []byte(tc.body), tc.encoding)
			header := http.Header{}
			if tc.contentType != "" {
				header.Set("Content-Type", tc.contentType)
			}
			if tc.encoding != "" {
				header.Set("Content-Encoding", tc.encoding)
			}
			response := &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(rawHTTPTestChunkReader{bytes.NewReader(wire)})}
			observer := ObserveRawHTTPResponse(context.Background(), nil, NewUsageReporter(context.Background(), "codex", "raw-model", nil), response, nil).(*rawHTTPResponseBody)
			got, err := io.ReadAll(observer)
			if err != nil {
				t.Fatal(err)
			}
			if err := observer.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wire) {
				t.Fatal("forwarded response bytes changed")
			}
			detail := observer.parser.detail
			if !tc.wantUsage {
				if detail != (usage.Detail{}) {
					t.Fatalf("usage = %+v", detail)
				}
				return
			}
			if detail.InputTokens != 100 || detail.OutputTokens != 12 || detail.TotalTokens != 112 || detail.CachedTokens != 80 || detail.CacheReadTokens != 80 || detail.ReasoningTokens != 4 {
				t.Fatalf("usage = %+v", detail)
			}
			if observer.reporter.responseModel != "served-model" {
				t.Fatalf("response model = %q", observer.reporter.responseModel)
			}
		})
	}
}

func TestRawHTTPObserverSniffDecision(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		wantSSE bool
	}{
		{name: "event field", prefix: "event: response.output_text.delta", wantSSE: true},
		{name: "data field", prefix: "data: {\"type\":\"response.created\"}", wantSSE: true},
		{name: "comment field", prefix: ":keepalive\n", wantSSE: true},
		{name: "leading crlf", prefix: "\r\nid: 1\ndata: {}", wantSSE: true},
		{name: "json object", prefix: "{\"usage\":{}}", wantSSE: false},
		{name: "json array", prefix: "[{\"usage\":{}}]", wantSSE: false},
		{name: "json after spaces", prefix: "   {\"usage\":{}}", wantSSE: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := newRawHTTPUsageParser(NewUsageReporter(context.Background(), "codex", "raw-model", nil), "")
			for _, b := range []byte(tc.prefix) {
				if _, err := parser.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if !parser.decided {
				t.Fatalf("parser stayed undecided for prefix %q", tc.prefix)
			}
			if parser.sse != tc.wantSSE {
				t.Fatalf("sse = %v, want %v", parser.sse, tc.wantSSE)
			}
			if len(parser.prefix) != 0 {
				t.Fatalf("buffered prefix was not flushed: %q", parser.prefix)
			}
		})
	}
}

func TestRawHTTPObserverSkipsOversizedEventsAndContinues(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "raw-model", nil)
	parser := &rawHTTPUsageParser{sse: true, decided: true, reporter: reporter}
	oversized := "data: " + strings.Repeat("x", rawHTTPUsageBufferLimit+32) + "\n\n"
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":42,\"input_tokens_details\":{\"cached_tokens\":32}}}}\n\n"
	_, _ = parser.Write([]byte(oversized + terminal))
	parser.finish()
	if parser.detail.InputTokens != 42 || parser.detail.CachedTokens != 32 {
		t.Fatalf("usage = %+v", parser.detail)
	}
	jsonParser := &rawHTTPUsageParser{decided: true, reporter: reporter}
	_, _ = jsonParser.Write([]byte(strings.Repeat(" ", rawHTTPUsageBufferLimit+1)))
	if !jsonParser.drop || len(jsonParser.data) != 0 {
		t.Fatal("oversized JSON telemetry was retained")
	}
}

func TestRawHTTPObserverHandlesSSELineEndingsAndDuplicateUsage(t *testing.T) {
	for _, ending := range []string{"\n", "\r", "\r\n"} {
		reporter := NewUsageReporter(context.Background(), "codex", "raw-model", nil)
		parser := &rawHTTPUsageParser{sse: true, decided: true, reporter: reporter}
		body := "event: response.completed" + ending + "data: {\"response\":{\"usage\":{\"input_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":7}}}}" + ending + ending
		for _, b := range []byte(body + body) {
			_, _ = parser.Write([]byte{b})
		}
		parser.finish()
		if parser.detail.InputTokens != 9 || parser.detail.CachedTokens != 7 {
			t.Fatalf("ending %q: usage = %+v", ending, parser.detail)
		}
	}
}

type rawHTTPTestReadError struct{ sent bool }

func (r *rawHTTPTestReadError) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "raw partial response"), io.ErrUnexpectedEOF
}
func (r *rawHTTPTestReadError) Close() error { return nil }

type rawHTTPBlockingTestBody struct {
	*io.PipeReader
	started chan struct{}
}

func (b *rawHTTPBlockingTestBody) Read(p []byte) (int, error) {
	close(b.started)
	return b.PipeReader.Read(p)
}

func TestRawHTTPObserverCloseUnblocksConcurrentReadAndDecoder(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "unsupported"} {
		t.Run(encoding, func(t *testing.T) {
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = writer.Close() })
			body := &rawHTTPBlockingTestBody{PipeReader: reader, started: make(chan struct{})}
			response := &http.Response{Header: http.Header{"Content-Encoding": {encoding}}, Body: body}
			observer := ObserveRawHTTPResponse(context.Background(), nil, NewUsageReporter(context.Background(), "codex", "raw-model", nil), response, nil)
			readDone := make(chan error, 1)
			go func() {
				_, err := observer.Read(make([]byte, 1))
				readDone <- err
			}()
			<-body.started
			closeDone := make(chan error, 1)
			go func() { closeDone <- observer.Close() }()
			select {
			case err := <-closeDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("close did not finish the blocked body and observation decoder")
			}
			select {
			case err := <-readDone:
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("read error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("read remained blocked after close")
			}
			if err := observer.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRawHTTPObserverPreservesReadErrorsAndOpaqueBodies(t *testing.T) {
	for _, encoding := range []string{"", "unsupported", "gzip"} {
		response := &http.Response{Header: http.Header{"Content-Encoding": {encoding}}, Body: &rawHTTPTestReadError{}}
		observer := ObserveRawHTTPResponse(context.Background(), nil, NewUsageReporter(context.Background(), "codex", "raw-model", nil), response, nil)
		got, err := io.ReadAll(observer)
		if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != "raw partial response" {
			t.Fatalf("body %q, error %v", got, err)
		}
		if err := observer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
