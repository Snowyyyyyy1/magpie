package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// setAgentsV1 turns settings.CodexAgentsV1 on or off as the Settings page's
// switch does, without provider's own helper (the agent package must not
// import provider back). The setting is process-wide, so it is put back when
// the test ends.
func setAgentsV1(t *testing.T, on bool) {
	t.Helper()
	was := settings.Load().CodexAgentsV1
	t.Cleanup(func() {
		s := settings.Load()
		s.CodexAgentsV1 = was
		if err := settings.Save(s); err != nil {
			t.Errorf("restoring settings.CodexAgentsV1: %v", err)
		}
	})
	s := settings.Load()
	s.CodexAgentsV1 = on
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
}

// codexAgentsHome is codexHome with magpie's own stash cleared when the test
// ends (it lives under the temp home, but the file is magpie's own and stays
// global).
func codexAgentsHome(t *testing.T, config string) (home string, read func() string) {
	t.Helper()
	home, read = codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, config)
	t.Cleanup(func() { os.Remove(stashPath()) })
	return home, read
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}

// route points Codex at one of magpie's models, as picking one does, and
// fails the test if the config isn't routed afterwards.
func route(t *testing.T, cx *Agent, read func() string) {
	t.Helper()
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), "openai_base_url") {
		t.Fatalf("not routed:\n%s", read())
	}
}

// Codex's own multi_agent_v2 explicitly on — the bare key or enabled = true in
// its table, spelled as a table, an inline object or a dotted key — turns the
// V1 magpie's models ask for off, so the Codex notice says so and magpie
// writes nothing: the config stays byte for byte, the user turns the key off
// (#141).
func TestCodexAgentsV1WarnsWhenCodexOwnV2IsOn(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"bool key", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n"},
		{"bool key comment", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true # codex v2\n"},
		{"table", "model = \"gpt-5.5\"\n\n[features.multi_agent_v2]\nenabled = true\nmax_concurrent_threads_per_session = 4\n"},
		{"inline", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = { enabled = true, max_concurrent_threads_per_session = 4 }\n"},
		{"dotted", "model = \"gpt-5.5\"\nfeatures.multi_agent_v2.enabled = true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, read := codexAgentsHome(t, tc.config)
			cx := codex(home)
			route(t, cx, read)
			setAgentsV1(t, true)
			before, bt := read(), modTime(t, cx.Path)
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if after := read(); after != before {
				t.Fatalf("magpie wrote to the config:\n%s\n→\n%s", before, after)
			}
			if mt := modTime(t, cx.Path); !mt.Equal(bt) {
				t.Fatalf("the config was rewritten: %v → %v", bt, mt)
			}
			if n := cx.Notice(); !strings.Contains(n, "multi_agent_v2") || !strings.Contains(n, "turn it off") {
				t.Fatalf("no notice that Codex's own V2 wins: %q", n)
			}
		})
	}
}

// Codex's own multi_agent_v2 off — absent, false, a table/inline object whose
// enabled is false or left out, or a string rather than a boolean — is Codex's
// default, which the V1 already is: no warning, and no write.
func TestCodexAgentsV1NoWarningWhenCodexOwnV2IsOff(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"absent", "model = \"gpt-5.5\"\n"},
		{"false", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = false\n"},
		{"table enabled false", "model = \"gpt-5.5\"\n\n[features.multi_agent_v2]\nenabled = false\n"},
		{"table enabled omitted", "model = \"gpt-5.5\"\n\n[features.multi_agent_v2]\nmax_concurrent_threads_per_session = 4\n"},
		{"inline enabled false", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = { enabled = false }\n"},
		{"inline enabled omitted", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = { max_concurrent_threads_per_session = 4 }\n"},
		{"bare string", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = \"true\"\n"},
		{"table string enabled", "model = \"gpt-5.5\"\n\n[features.multi_agent_v2]\nenabled = \"true\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, read := codexAgentsHome(t, tc.config)
			cx := codex(home)
			route(t, cx, read)
			setAgentsV1(t, true)
			before := read()
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if after := read(); after != before {
				t.Fatalf("magpie wrote to the config:\n%s\n→\n%s", before, after)
			}
			if n := cx.Notice(); strings.Contains(n, "multi_agent_v2") {
				t.Fatalf("a warning with Codex's own V2 off: %q", n)
			}
		})
	}
}

// The setting off, or Codex not routed through magpie, is nothing to warn
// about even with Codex's own V2 on: the setting has no effect there.
func TestCodexAgentsV1NoWarningOffOrUnrouted(t *testing.T) {
	// the setting off, Codex routed, its own V2 on
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	route(t, cx, read)
	setAgentsV1(t, false)
	if n := cx.Notice(); strings.Contains(n, "multi_agent_v2") {
		t.Fatalf("a warning with the setting off: %q", n)
	}
	// the setting on, Codex not routed
	home2, _ := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx2 := codex(home2)
	setAgentsV1(t, true)
	if n := cx2.Notice(); strings.Contains(n, "multi_agent_v2") {
		t.Fatalf("a warning with Codex unrouted: %q", n)
	}
}

// A features key of a shape magpie doesn't take for a boolean or a table — an
// array table in [features]' place — is not a conflict: no warning, no write,
// and the rest of the sync still runs on the config left exactly as it was.
func TestCodexAgentsV1UnexpectedFeaturesShapeNotBlocked(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[[features]]\nname = \"x\"\n")
	cx := codex(home)
	route(t, cx, read)
	setAgentsV1(t, true)
	before := read()
	if err := cx.Sync(); err != nil {
		t.Fatalf("an unexpected features shape blocked Sync: %v", err)
	}
	if after := read(); after != before {
		t.Fatalf("the config was rewritten:\n%s\n→\n%s", before, after)
	}
	if n := cx.Notice(); strings.Contains(n, "multi_agent_v2") {
		t.Fatalf("a conflict warning for a shape magpie doesn't read: %q", n)
	}
}

// A config the TOML parser refuses is a read error for the notice too: told as
// such rather than taken for a conflict or read as absent.
func TestCodexAgentsV1UnparseableConfigNotice(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n")
	cx := codex(home)
	route(t, cx, read)
	setAgentsV1(t, true)
	if err := os.WriteFile(cx.Path, []byte(read()+"\noops =\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := cx.Notice(); !strings.Contains(n, "couldn't read") {
		t.Fatalf("unparseable config: %q", n)
	}
}

// CodexSubAgentsV1Warning is the same warning for the Settings row, found from
// the Codex config here rather than a path the caller already has.
func TestCodexSubAgentsV1WarningSetting(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	route(t, cx, read)
	setAgentsV1(t, true)
	if n := CodexSubAgentsV1Warning(); !strings.Contains(n, "multi_agent_v2") {
		t.Fatalf("setting row warning: %q", n)
	}
	setAgentsV1(t, false)
	if n := CodexSubAgentsV1Warning(); n != "" {
		t.Fatalf("setting row warning with the setting off: %q", n)
	}
}
