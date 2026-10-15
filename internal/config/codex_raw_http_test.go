package config

import "testing"

func TestCodexRawHTTPPassthroughConfigRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"upstream: {codex: {raw-http-passthrough: true}}",
		"codex: {raw-http-passthrough: true}",
		"oauth: {providers: {codex: {raw-http-passthrough: true}}}",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Codex.RawHTTPPassthrough || !cfg.ForAPIKey().Codex.RawHTTPPassthrough {
				t.Fatal("raw HTTP setting was lost in credential-scoped views")
			}
			migrated, _, err := NormalizeConfigLayout([]byte(raw), true)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateV8Config(migrated); err != nil {
				t.Fatal(err)
			}
			restored, err := ParseConfigBytes(migrated)
			if err != nil {
				t.Fatal(err)
			}
			if !restored.Codex.RawHTTPPassthrough {
				t.Fatal("raw HTTP setting was lost during migration")
			}
		})
	}
	cfg, err := ParseConfigBytes([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.RawHTTPPassthrough {
		t.Fatal("raw HTTP passthrough must be opt-in")
	}
}
