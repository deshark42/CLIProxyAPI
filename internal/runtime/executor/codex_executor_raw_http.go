package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// RawResponses bypasses protocol adapters and returns the upstream HTTP response intact.
// semanticBody is a read-only decoded copy used for routing and payload-rule validation.
func (e *CodexExecutor) RawResponses(ctx context.Context, auth *cliproxyauth.Auth, incoming *http.Request, semanticBody []byte) (response *http.Response, err error) {
	model := gjson.GetBytes(semanticBody, "model").String()
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	reporter.SetStream(gjson.GetBytes(semanticBody, "stream").Bool())
	reporter.SetTranslatedReasoningEffort(semanticBody, sdktranslator.FormatCodex.String())
	defer reporter.TrackFailure(ctx, &err)
	apiKey, baseURL := codexCreds(auth)
	if err := ensureCodexSIWCCredentials(auth, apiKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("codex raw HTTP passthrough: credential has no access token")
	}
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	suffix := "/responses"
	if strings.HasSuffix(incoming.URL.Path, "/responses/compact") {
		suffix += "/compact"
	}
	target, err := url.Parse(strings.TrimRight(baseURL, "/") + suffix)
	if err != nil {
		return nil, err
	}
	target.RawQuery = helps.StripRawHTTPProxyQueryCredentials(incoming.URL.RawQuery)
	upstream := incoming.Clone(ctx)
	upstream.URL = target
	upstream.Host = target.Host
	upstream.RequestURI = ""
	upstream.Header = incoming.Header.Clone()
	upstream.TransferEncoding = nil
	upstream.Close = false
	cliproxyexecutor.StripHopByHopHeaders(upstream.Header)
	// Never leak the proxy's API key to the credential provider.
	upstream.Header.Del("X-Api-Key")
	upstream.Header.Del("X-Goog-Api-Key")
	upstream.Header.Set("Authorization", "Bearer "+apiKey)
	upstream.Header.Del("Chatgpt-Account-Id")
	if !codexAuthUsesAPIKey(auth) && !codexUseSIWC(auth) && auth != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok && accountID != "" {
			upstream.Header.Set("Chatgpt-Account-Id", accountID)
		}
	}
	// A present empty value suppresses Go's implicit User-Agent without adding an identity.
	if _, ok := upstream.Header["User-Agent"]; !ok {
		upstream.Header["User-Agent"] = nil
	}
	// The Codex backend keys prompt caching on Session-Id, not on the body's
	// prompt_cache_key. Ingresses that drop underscore headers would otherwise strip the
	// caller's Session_id and force a fresh cache key on every request. Headers only; the
	// body stays byte-identical.
	helps.EnsureCodexSessionHeader(upstream.Header, semanticBody)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: semanticBody, Headers: incoming.Header, Query: incoming.URL.Query(), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: incoming.URL.Path}}
	request := cliproxyexecutor.Request{Model: model, Payload: semanticBody}
	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), model, sdktranslator.FormatCodex.String(), "", semanticBody, request, opts)
	client, err := helps.NewRawHTTPClient(ctx, e.cfg, auth)
	if err != nil {
		return nil, err
	}
	// Opaque requests cannot prove that configured rules leave the business payload intact.
	if helps.RawHTTPPayloadInspectionRequired(e.cfg, incoming.URL.Path) && (!gjson.ValidBytes(semanticBody) || strings.TrimSpace(model) == "") {
		return nil, statusErr{code: http.StatusBadRequest, msg: "raw HTTP passthrough cannot validate payload rules for an opaque request or missing model"}
	}
	// Apply user rules exactly once at the final semantic barrier. Fail closed if
	// they conflict with passthrough; never silently send a modified request.
	if finalBody := finalize(append([]byte(nil), semanticBody...)); !bytes.Equal(finalBody, semanticBody) {
		return nil, statusErr{code: http.StatusBadRequest, msg: "raw HTTP passthrough conflicts with payload rules that modify the request"}
	}
	if e.cfg != nil && e.cfg.RequestLog {
		wireBody, errRead := io.ReadAll(upstream.Body)
		if errRead != nil {
			return nil, errRead
		}
		upstream.Body = io.NopCloser(bytes.NewReader(wireBody))
		var authID, authLabel, authType, authValue string
		if auth != nil {
			authID, authLabel = auth.ID, auth.Label
			authType, authValue = auth.AccountInfo()
		}
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL: upstream.URL.String(), Method: upstream.Method, Headers: upstream.Header.Clone(),
			Body: wireBody, Provider: e.Identifier(), AuthID: authID, AuthLabel: authLabel,
			AuthType: authType, AuthValue: authValue,
		})
	}
	client = reporter.TrackHTTPClient(client)
	response, err = client.Do(upstream)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, response.Header.Clone())
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		reporter.SetStream(true)
	}
	var upstreamErr error
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		upstreamErr = statusErr{code: response.StatusCode, msg: http.StatusText(response.StatusCode)}
	}
	response.Body = helps.ObserveRawHTTPResponse(ctx, e.cfg, reporter, response, upstreamErr, func(payload []byte) error {
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return newCodexStatusErrWithCooling(response.StatusCode, payload, e.modelLevelCooling())
		}
		if failure, _, ok := codexTerminalFailureErrWithCooling(payload, e.modelLevelCooling()); ok {
			return failure
		}
		return nil
	})
	return response, nil
}

func (e *CodexAutoExecutor) RawResponses(ctx context.Context, auth *cliproxyauth.Auth, incoming *http.Request, semanticBody []byte) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex raw HTTP passthrough: executor is nil")
	}
	return e.httpExec.RawResponses(ctx, auth, incoming, semanticBody)
}
