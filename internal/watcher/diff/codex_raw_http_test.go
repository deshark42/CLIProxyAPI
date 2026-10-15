package diff

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"testing"
)

func TestBuildConfigChangeDetailsCodexRawHTTPPassthrough(t *testing.T) {
	oldCfg, newCfg := &config.Config{}, &config.Config{}
	newCfg.Codex.RawHTTPPassthrough = true
	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	if len(changes) != 1 || changes[0] != "upstream.codex.raw-http-passthrough: false -> true" {
		t.Fatalf("changes = %v", changes)
	}
	if changes := BuildConfigChangeDetails(newCfg, newCfg); len(changes) != 0 {
		t.Fatalf("unchanged setting produced changes: %v", changes)
	}
}
