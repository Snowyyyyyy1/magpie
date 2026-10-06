package gui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agentenv"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Settings' Codex subagents V1 (#141) reaches Codex's config through the
// agents' sync. When it can't — here, Codex isn't connected to magpie — the
// answer says so rather than implying the override landed, and turning it
// off has nothing of the setting's to say.
func TestCodexAgentsV1SettingNotice(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	t.Setenv("APPDATA", filepath.Join(h, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(h, "AppData", "Local"))
	for _, v := range agentenv.Vars {
		t.Setenv(v, "")
	}
	// both are process-wide and must go back even when the test fails
	was := settings.Load().CodexAgentsV1
	t.Cleanup(func() {
		s := settings.Load()
		s.CodexAgentsV1 = was
		if err := settings.Save(s); err != nil {
			t.Errorf("restoring settings.CodexAgentsV1: %v", err)
		}
		os.Remove(filepath.Join(filepath.Dir(provider.Path()), "stash.json"))
	})
	call := func(body string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		Handler(nil, nil).ServeHTTP(rec, httptest.NewRequest("POST", "/api/settings/codex-agents-v1", strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := call(`{"on":true}`)
	if code != 200 || out["codexAgentsV1"] != true || !settings.Load().CodexAgentsV1 {
		t.Fatalf("on: %d %v", code, out["codexAgentsV1"])
	}
	if n, _ := out["notice"].(string); !strings.Contains(n, "multi-agent V1") {
		t.Fatalf("no notice that the override didn't land: %q", n)
	}
	code, out = call(`{"on":false}`)
	if code != 200 || settings.Load().CodexAgentsV1 {
		t.Fatalf("off: %d %v", code, settings.Load().CodexAgentsV1)
	}
	if n, _ := out["notice"].(string); strings.Contains(n, "multi-agent V1") {
		t.Fatalf("off notice about the setting: %q", n)
	}
}
