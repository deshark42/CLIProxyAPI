package executor

import (
	"bytes"
	"context"
	"fmt"
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
func (e *CodexExecutor) RawResponses(ctx context.Context, auth *cliproxyauth.Auth, incoming *http.Request, semanticBody []byte) (*http.Response, error) {
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
	target.RawQuery = incoming.URL.RawQuery
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
	model := gjson.GetBytes(semanticBody, "model").String()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: semanticBody, Headers: incoming.Header, Query: incoming.URL.Query(), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: incoming.URL.Path}}
	request := cliproxyexecutor.Request{Model: model, Payload: semanticBody}
	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), model, sdktranslator.FormatCodex.String(), "", semanticBody, request, opts)
	client, err := helps.NewRawHTTPClient(ctx, e.cfg, auth)
	if err != nil {
		return nil, err
	}
	// Apply user rules exactly once at the final semantic barrier. Fail closed if
	// they conflict with passthrough; never silently send a modified request.
	if finalBody := finalize(append([]byte(nil), semanticBody...)); !bytes.Equal(finalBody, semanticBody) {
		return nil, statusErr{code: http.StatusBadRequest, msg: "raw HTTP passthrough conflicts with payload rules that modify the request"}
	}
	return client.Do(upstream)
}

func (e *CodexAutoExecutor) RawResponses(ctx context.Context, auth *cliproxyauth.Auth, incoming *http.Request, semanticBody []byte) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex raw HTTP passthrough: executor is nil")
	}
	return e.httpExec.RawResponses(ctx, auth, incoming, semanticBody)
}
