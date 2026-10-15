package handlers

import (
	"context"
	"net/http"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// RoundTripRawCodexResponses preserves caller metadata but bypasses payload interceptors.
func (h *BaseAPIHandler) RoundTripRawCodexResponses(ctx context.Context, req *http.Request, semanticBody []byte) (*http.Response, error) {
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: requestExecutionMetadata(ctx)}
	opts.Metadata[coreexecutor.RequestPathMetadataKey] = req.URL.Path
	return h.AuthManager.RoundTripCodexResponses(ctx, req, semanticBody, opts)
}
