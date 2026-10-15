package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
	} else {
		result.Success = response.StatusCode < http.StatusBadRequest
		if !result.Success {
			result.Error = &Error{HTTPStatus: response.StatusCode, Message: http.StatusText(response.StatusCode)}
		}
	}
	m.MarkResult(ctx, result)
	return response, err
}
