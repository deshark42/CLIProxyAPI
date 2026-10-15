package helps

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// This bounds telemetry buffers only; oversized bodies and events are still forwarded.
const rawHTTPUsageBufferLimit = 4 << 20

// ObserveRawHTTPResponse records a response without changing the bytes read by its caller.
// The returned body must be closed, including when downstream forwarding is interrupted.
func ObserveRawHTTPResponse(ctx context.Context, cfg *config.Config, reporter *UsageReporter, response *http.Response, upstreamErr error) io.ReadCloser {
	observer := &rawHTTPResponseBody{
		body: response.Body, ctx: ctx, cfg: cfg, reporter: reporter, upstreamErr: upstreamErr,
		parser: &rawHTTPUsageParser{reporter: reporter, sse: strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")},
	}
	encoding := strings.TrimSpace(strings.ToLower(response.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" {
		reader, writer := io.Pipe()
		observer.encoded = writer
		observer.decoded = make(chan struct{})
		go func() {
			defer close(observer.decoded)
			defer func() { _ = reader.Close() }()
			if err := observeRawHTTPEncoded(reader, encoding, observer.parser); err != nil {
				// Decoding failures affect observability only, never HTTP forwarding or its outcome.
				log.WithError(err).Debug("raw HTTP usage observation stopped")
			}
		}()
	}
	return observer
}

type rawHTTPResponseBody struct {
	body        io.ReadCloser
	ctx         context.Context
	cfg         *config.Config
	reporter    *UsageReporter
	upstreamErr error
	parser      *rawHTTPUsageParser
	encoded     *io.PipeWriter
	decoded     chan struct{}
	mu          sync.Mutex
	finished    bool
}

func (b *rawHTTPResponseBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.finished {
		if n > 0 {
			AppendAPIResponseChunk(b.ctx, b.cfg, p[:n])
			if b.encoded != nil {
				_, _ = b.encoded.Write(p[:n])
			} else {
				_, _ = b.parser.Write(p[:n])
			}
		}
		if err != nil {
			if err == io.EOF {
				b.finish(nil)
			} else {
				b.finish(err)
			}
		}
	}
	return n, err
}

func (b *rawHTTPResponseBody) Close() error {
	err := b.body.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.finished {
		failure := err
		if failure == nil {
			failure = b.ctx.Err()
		}
		if failure == nil {
			failure = io.ErrUnexpectedEOF
		}
		b.finish(failure)
	}
	return err
}

func (b *rawHTTPResponseBody) finish(readErr error) {
	b.finished = true
	if b.encoded != nil {
		_ = b.encoded.Close()
		<-b.decoded
	}
	b.parser.finish()
	failure := b.upstreamErr
	if failure == nil {
		failure = readErr
	}
	if failure == nil {
		failure = b.parser.failure
	}
	if failure != nil {
		RecordAPIResponseError(b.ctx, b.cfg, failure)
		b.reporter.PublishFailureWithDetail(b.ctx, b.parser.detail, failure)
	} else {
		b.reporter.Publish(b.ctx, b.parser.detail)
	}
}

func observeRawHTTPEncoded(reader io.Reader, encoding string, parser *rawHTTPUsageParser) error {
	var closers []io.Closer
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			if err := closers[i].Close(); err != nil {
				log.WithError(err).Debug("raw HTTP observation decoder close")
			}
		}
	}()
	encodings := strings.Split(encoding, ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		switch strings.TrimSpace(encodings[i]) {
		case "identity":
		case "gzip", "x-gzip":
			decoder, err := gzip.NewReader(reader)
			if err != nil {
				return err
			}
			reader = decoder
			closers = append(closers, decoder)
		case "deflate":
			decoder, err := zlib.NewReader(reader)
			if err != nil {
				return err
			}
			reader = decoder
			closers = append(closers, decoder)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
			if err != nil {
				return err
			}
			defer decoder.Close()
			reader = decoder
		default:
			return fmt.Errorf("unsupported raw HTTP observation content encoding %q", encodings[i])
		}
	}
	_, err := io.Copy(parser, reader)
	return err
}

type rawHTTPUsageParser struct {
	reporter   *UsageReporter
	sse        bool
	line       []byte
	data       []byte
	drop       bool
	previousCR bool
	detail     usage.Detail
	failure    error
}

func (p *rawHTTPUsageParser) Write(chunk []byte) (int, error) {
	if !p.sse {
		if !p.drop {
			if len(chunk) > rawHTTPUsageBufferLimit-len(p.data) {
				p.data = nil
				p.drop = true
			} else {
				p.data = append(p.data, chunk...)
			}
		}
		return len(chunk), nil
	}
	for _, ch := range chunk {
		if p.previousCR && ch == '\n' {
			p.previousCR = false
			continue
		}
		p.previousCR = ch == '\r'
		if ch == '\r' || ch == '\n' {
			p.consumeLine()
		} else if len(p.line) < rawHTTPUsageBufferLimit {
			p.line = append(p.line, ch)
		} else {
			p.drop = true
		}
	}
	return len(chunk), nil
}

func (p *rawHTTPUsageParser) consumeLine() {
	if len(p.line) == 0 {
		if !p.drop {
			p.observe(p.data)
		}
		p.data = p.data[:0]
		p.drop = false
	} else if !p.drop && bytes.HasPrefix(p.line, []byte("data:")) {
		value := p.line[len("data:"):]
		value = bytes.TrimPrefix(value, []byte(" "))
		if len(value)+1 > rawHTTPUsageBufferLimit-len(p.data) {
			p.drop = true
		} else {
			p.data = append(p.data, value...)
			p.data = append(p.data, '\n')
		}
	}
	p.line = p.line[:0]
}

func (p *rawHTTPUsageParser) finish() {
	if p.sse && len(p.line) > 0 {
		p.consumeLine()
	}
	if !p.drop {
		p.observe(p.data)
	}
	p.line = nil
	p.data = nil
}

func (p *rawHTTPUsageParser) observe(payload []byte) {
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	if model := root.Get("model"); !p.sse && model.Type == gjson.String {
		p.reporter.SetResponseModel(model.String())
	} else if root.Get("type").String() == "response.failed" {
		if model := root.Get("response.model"); model.Type == gjson.String {
			p.reporter.SetResponseModel(model.String())
		}
	}
	p.reporter.ObserveResponseModel(payload)
	if node := root.Get("response.usage"); hasOpenAIStyleUsageTokenFields(node) {
		p.detail, _ = ParseCodexUsage(payload)
	} else if node := root.Get("usage"); hasOpenAIStyleUsageTokenFields(node) {
		p.detail = ParseOpenAIUsage(payload)
	}
	if tier := extractResponseServiceTier(payload); tier != "" {
		p.detail.ResponseServiceTier = tier
	}
	switch root.Get("type").String() {
	case "error", "response.failed":
		p.failure = fmt.Errorf("raw upstream event: %s", root.Get("type").String())
	}
}
