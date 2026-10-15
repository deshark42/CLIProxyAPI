package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type rawCodexResponsesExecutor interface {
	RawResponses(context.Context, *Auth, *http.Request, []byte) (*http.Response, error)
}

// CodexRawHTTPPassthroughEnabled reads the hot-reloadable upstream setting.
func (m *Manager) CodexRawHTTPPassthroughEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Codex.RawHTTPPassthrough
}

// RoundTripCodexResponses selects and prepares one Codex credential without
// model rewriting, plugin interceptors, response conversion, or upstream retries.
func (m *Manager) RoundTripCodexResponses(ctx context.Context, request *http.Request, semanticBody []byte, opts cliproxyexecutor.Options) (*http.Response, error) {
	if m.HomeEnabled() {
		return nil, fmt.Errorf("raw HTTP passthrough is not supported with Home dispatch")
	}
	model := gjson.GetBytes(semanticBody, "model").String()
	opts.Headers = request.Header.Clone()
	opts.OriginalRequest = semanticBody
	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey] = model
	auth, executor, err := m.pickNext(ctx, "codex", model, opts, map[string]struct{}{})
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(auth.Provider, "codex") {
		return nil, fmt.Errorf("raw HTTP passthrough requires a Codex credential")
	}
	if rt := m.roundTripperFor(auth); rt != nil {
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	auth, err = m.prepareRequestAuth(ctx, executor, auth)
	if err != nil {
		return nil, err
	}
	executor = executorForAuth(executor, auth)
	raw, ok := executor.(rawCodexResponsesExecutor)
	if !ok {
		return nil, fmt.Errorf("Codex executor does not support raw HTTP passthrough")
	}
	publishSelectedAuthMetadata(opts.Metadata, auth)
	response, err := raw.RawResponses(ctx, auth, request, semanticBody)
	result := Result{AuthID: auth.ID, Provider: "codex", Model: model, RouteModel: model, Options: opts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch}
	if err != nil {
		result.Error = resultErrorFromError(err)
		result.RetryAfter = retryAfterFromError(err)
		result.CredentialScope = isCredentialScopedError(err)
		m.MarkResult(ctx, result)
		return response, err
	}
	// HTTP error bodies carry request scope and quota reset hints. Wait for observation
	// to finish, just as for SSE, without reading ahead or delaying response forwarding.
	response.Body = &rawCodexResultBody{ReadCloser: response.Body, complete: func(bodyErr error) {
		if bodyErr == nil && response.StatusCode >= http.StatusBadRequest {
			bodyErr = &Error{HTTPStatus: response.StatusCode, Message: http.StatusText(response.StatusCode)}
		}
		result.Success = bodyErr == nil
		if bodyErr != nil {
			result.Error = resultErrorFromError(bodyErr)
			result.RetryAfter = retryAfterFromError(bodyErr)
			result.CredentialScope = isCredentialScopedError(bodyErr)
			if delay := rawHTTPRetryAfter(response.Header.Get("Retry-After"), time.Now()); delay != nil {
				result.RetryAfter = delay
			}
		}
		m.MarkResult(ctx, result)
	}}
	return response, nil
}

func rawHTTPRetryAfter(value string, now time.Time) *time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
			delay := time.Duration(seconds) * time.Second
			return &delay
		}
		return nil
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		delay := deadline.Sub(now)
		return &delay
	}
	return nil
}

type rawCodexResultBody struct {
	io.ReadCloser
	once     sync.Once
	complete func(error)
}

func (b *rawCodexResultBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		failure := err
		if err == io.EOF {
			failure = nil
		}
		b.finish(failure)
	}
	return n, err
}

func (b *rawCodexResultBody) Close() error {
	err := b.ReadCloser.Close()
	failure := err
	if failure == nil {
		failure = io.ErrUnexpectedEOF
	}
	b.finish(failure)
	return err
}

func (b *rawCodexResultBody) finish(failure error) {
	b.once.Do(func() {
		if observed, ok := b.ReadCloser.(interface{ RawHTTPResponseError() error }); ok {
			if observedErr := observed.RawHTTPResponseError(); observedErr != nil {
				failure = observedErr
			}
		}
		b.complete(failure)
	})
}
