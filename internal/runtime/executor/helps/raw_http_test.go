package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRawHTTPPayloadInspectionRequired(t *testing.T) {
	if RawHTTPPayloadInspectionRequired(nil, "/v1/responses") {
		t.Fatal("nil config requires inspection")
	}
	for _, tc := range []struct {
		name string
		mode config.DisableImageGenerationMode
		path string
		want bool
	}{
		{"off", config.DisableImageGenerationOff, "/v1/responses", false},
		{"all", config.DisableImageGenerationAll, "/v1/responses", true},
		{"chat", config.DisableImageGenerationChat, "/v1/responses", true},
		{"chat-compact", config.DisableImageGenerationChat, "/v1/responses/compact", true},
		{"chat-images", config.DisableImageGenerationChat, "/v1/images/generations", false},
		{"passthrough", config.DisableImageGenerationPassthrough, "/v1/responses", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.DisableImageGeneration = tc.mode
			if got := RawHTTPPayloadInspectionRequired(cfg, tc.path); got != tc.want {
				t.Fatalf("inspection required=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestRawHTTPProxyQueryCredentials(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""},
		{"custom=a%2fb&custom=2&x=a+b&x=a%20b", "custom=a%2fb&custom=2&x=a+b&x=a%20b"},
		{"key=secret&key=other&auth_token=secret", ""},
		{"%6bey=secret&auth%5ftoken=secret&keep=%FF", "keep=%FF"},
		{"key&keep=&auth_token", "keep="},
		{"a=1&&key=secret&b=2&", "a=1&&b=2&"},
		{"bad%ZZ=keep&Key=keep&api_key=keep&key=secret", "bad%ZZ=keep&Key=keep&api_key=keep"},
	} {
		if got := StripRawHTTPProxyQueryCredentials(tc.input); got != tc.want {
			t.Errorf("sanitize %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}
