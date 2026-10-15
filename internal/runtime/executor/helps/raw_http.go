package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var rawHTTPTransports = NewTransportCache[string](DefaultTransportCacheCapacity)

// StripRawHTTPProxyQueryCredentials preserves the encoding and order of non-auth parameters.
func StripRawHTTPProxyQueryCredentials(rawQuery string) string {
	parts := strings.Split(rawQuery, "&")
	kept := parts[:0]
	for _, part := range parts {
		name, _, _ := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(name)
		if err == nil && (decoded == "key" || decoded == "auth_token") {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "&")
}

// RawHTTPPayloadInspectionRequired reports whether opaque requests must fail closed.
func RawHTTPPayloadInspectionRequired(cfg *config.Config, requestPath string) bool {
	if cfg == nil {
		return false
	}
	rules := cfg.Payload
	return shouldStripImageGeneration(cfg.DisableImageGeneration, requestPath) || len(rules.Default) != 0 || len(rules.DefaultRaw) != 0 ||
		len(rules.Override) != 0 || len(rules.OverrideRaw) != 0 || len(rules.Filter) != 0
}

// NewRawHTTPClient disables implicit compression, redirects, and payload decorators.
// Unknown transports are rejected rather than silently weakening byte preservation.
func NewRawHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*http.Client, error) {
	proxyURL := effectiveProxyURL(ctx, cfg, auth)
	var base *http.Transport
	key := proxyURL
	if proxyURL == "" {
		rt, _ := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
		if rt == nil {
			rt = http.DefaultTransport
		}
		var ok bool
		base, ok = rt.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("raw HTTP passthrough requires an http.Transport, got %T", rt)
		}
		key = fmt.Sprintf("transport:%p", base)
	}
	tr, err := rawHTTPTransports.Get(key, func() (*http.Transport, error) {
		if proxyURL != "" {
			base = buildProxyTransport(proxyURL)
			if base == nil {
				return nil, fmt.Errorf("raw HTTP passthrough: invalid proxy configuration")
			}
		}
		tr := base.Clone()
		tr.DisableCompression = true
		tr.ResponseHeaderTimeout = 0
		return tr, nil
	})
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
