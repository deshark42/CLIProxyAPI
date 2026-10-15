package openai

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func (h *OpenAIResponsesAPIHandler) rawHTTPPassthroughEnabled() bool {
	return h != nil && h.BaseAPIHandler != nil && h.AuthManager != nil && h.AuthManager.CodexRawHTTPPassthroughEnabled()
}

func (h *OpenAIResponsesAPIHandler) handleRawHTTPResponse(c *gin.Context) {
	raw, err := c.GetRawData()
	if err != nil {
		h.writeRawHTTPError(c, err, http.StatusBadRequest)
		return
	}
	// Inspect a decoded copy for routing only; the original wire bytes are sent.
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	semanticBody, err := handlers.ReadRequestBody(c)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			h.writeRawHTTPError(c, err, http.StatusRequestEntityTooLarge)
			return
		}
		// Unsupported or malformed encodings are opaque, not proxy errors.
		// Select a credential without a model hint and let Codex decide.
		semanticBody = raw
	}
	request := c.Request.Clone(c.Request.Context())
	request.Body = io.NopCloser(bytes.NewReader(raw))
	request.ContentLength = int64(len(raw))
	request.GetBody = nil
	ctx, cancel := h.GetContextWithCancel(h, c, c.Request.Context())
	defer cancel()
	response, err := h.RoundTripRawCodexResponses(ctx, request, semanticBody)
	if err != nil {
		h.writeRawHTTPError(c, err, http.StatusBadGateway)
		return
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.Debugf("raw Codex response close: %v", err)
		}
	}()
	header := response.Header.Clone()
	coreexecutor.StripHopByHopHeaders(header)
	for name, values := range header {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
	for name := range response.Trailer {
		c.Writer.Header().Add("Trailer", name)
	}
	c.Status(response.StatusCode)
	c.Writer.WriteHeaderNow()
	if err := http.NewResponseController(c.Writer).Flush(); err != nil && err != http.ErrNotSupported {
		log.Debugf("raw Codex response header flush: %v", err)
		return
	}
	if _, err := io.Copy(rawHTTPResponseWriter{writer: c.Writer}, response.Body); err != nil {
		// Once committed, never append a proxy-generated JSON or SSE error.
		log.Debugf("raw Codex response copy: %v", err)
		return
	}
	for name, values := range response.Trailer {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
}

type rawHTTPResponseWriter struct{ writer http.ResponseWriter }

func (w rawHTTPResponseWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil {
		return n, err
	}
	if err := http.NewResponseController(w.writer).Flush(); err != nil && err != http.ErrNotSupported {
		return n, err
	}
	return n, nil
}

func (h *OpenAIResponsesAPIHandler) writeRawHTTPError(c *gin.Context, err error, fallbackStatus int) {
	c.JSON(clienterror.HTTPStatusFromErrorOr(err, fallbackStatus), handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: fmt.Sprintf("Raw Codex passthrough: %v", err), Type: "api_error"}})
}
