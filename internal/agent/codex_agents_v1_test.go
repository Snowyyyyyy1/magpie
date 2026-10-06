package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
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

// codexAgentsHome is codexHome with the stash cleared when the test ends (it
// lives under the temp home, but the file is magpie's own and stays global).
func codexAgentsHome(t *testing.T, config string) (home string, read func() string) {
	t.Helper()
	home, read = codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, config)
	t.Cleanup(func() { os.Remove(stashPath()) })
	return home, read
}

func feature(t *testing.T, cx *Agent, key string) (string, bool) {
	t.Helper()
	tb, err := edit.GetTOMLTable(cx.Path, "features")
	if err != nil {
		t.Fatal(err)
	}
	if tb == nil {
		return "", false
	}
	v, ok := tb[key]
	return v, ok
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}

// readOnly makes a folder refuse new files (what a config write makes beside
// it), skipping where the folder is writable anyway (root).
func readOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if f, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		f.Close()
		os.Remove(filepath.Join(dir, "probe"))
		t.Skip("a read-only folder is writable here (root?)")
	}
}

func makeWritable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// Turning settings.CodexAgentsV1 on writes features.multi_agent_v2 = false
// into the Codex config magpie routes — the entries' "v1" alone is not
// enough, Codex's own V2 setting still wins — remembering what the user had
// so turning it off puts that back (#141).
func TestCodexAgentsV1FeatureOverride(t *testing.T) {
	for _, tc := range []struct {
		name, config, wantBack string
		absentBack             bool
	}{
		{"absent", "model = \"gpt-5.5\"\n", "", true},
		{"false", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = false\n", "false", false},
		{"true", "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n", "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, read := codexAgentsHome(t, tc.config)
			cx := codex(home)
			// routed through magpie on one of its models
			if err := cx.Fields[0].Set("fake/m1"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(read(), "openai_base_url") {
				t.Fatalf("not routed:\n%s", read())
			}
			setAgentsV1(t, true)
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "false" {
				t.Fatalf("on: features.multi_agent_v2 = %q, %v\n%s", v, ok, read())
			}
			// a second sync writes nothing more: the bytes stay and the file
			// isn't rewritten (its time doesn't move)
			before, bt := read(), modTime(t, cx.Path)
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if after := read(); after != before {
				t.Fatalf("sync wasn't idempotent:\n%s\n%s", before, after)
			}
			if mt := modTime(t, cx.Path); !mt.Equal(bt) {
				t.Fatalf("an unchanged value was written again: %v → %v", bt, mt)
			}
			setAgentsV1(t, false)
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			v, ok := feature(t, cx, "multi_agent_v2")
			if tc.absentBack {
				if ok {
					t.Fatalf("off: key stayed: %q\n%s", v, read())
				}
			} else if !ok || v != tc.wantBack {
				t.Fatalf("off: features.multi_agent_v2 = %q, %v, want %q\n%s", v, ok, tc.wantBack, read())
			}
		})
	}
}

// Unwiring Codex puts back the user's own multi_agent_v2 even where the
// route is removed with it, and one picked as its own (set "") does too.
func TestCodexAgentsV1FeatureRestoredOnUnwire(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  func(a *Agent) error
	}{
		{"unwire", func(a *Agent) error { return a.Unwire() }},
		{"own model", func(a *Agent) error { return a.Fields[0].Set("gpt-5.5") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
			cx := codex(home)
			if err := cx.Fields[0].Set("fake/m1"); err != nil {
				t.Fatal(err)
			}
			setAgentsV1(t, true)
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if v, _ := feature(t, cx, "multi_agent_v2"); v != "false" {
				t.Fatalf("on: %q\n%s", v, read())
			}
			if err := tc.out(cx); err != nil {
				t.Fatal(err)
			}
			if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
				t.Fatalf("out: features.multi_agent_v2 = %q, %v\n%s", v, ok, read())
			}
		})
	}
}

// A Codex config magpie does not route is left alone: the switch stamps the
// entries magpie serves, but writes nothing into a config that never talks
// to magpie.
func TestCodexAgentsV1LeavesUnroutedConfig(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n")
	cx := codex(home)
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); ok {
		t.Fatalf("an unrouted config was written: %q\n%s", v, read())
	}
}

// The user's other [features] keys stay as they are through on and off.
func TestCodexAgentsV1KeepsOtherFeatures(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nsomething_else = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "something_else"); !ok || v != "true" {
		t.Fatalf("another key lost: %q, %v\n%s", v, ok, read())
	}
	setAgentsV1(t, false)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "something_else"); !ok || v != "true" {
		t.Fatalf("another key lost on off: %q, %v\n%s", v, ok, read())
	}
	if _, ok := feature(t, cx, "multi_agent_v2"); ok {
		t.Fatalf("key not taken out on off:\n%s", read())
	}
}

// A value the user writes in by hand while the setting is on is theirs:
// repeated syncs leave it, the setting off doesn't put magpie's record back
// over it, and a new enable captures the value there then.
func TestCodexAgentsV1ManualEditIsKept(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, _ := feature(t, cx, "multi_agent_v2"); v != "false" {
		t.Fatalf("on: %q\n%s", v, read())
	}
	// the user flips it back on by hand while the setting is on
	if err := edit.SetTOMLKey(cx.Path, "features", "multi_agent_v2", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := cx.Sync(); err != nil {
			t.Fatal(err)
		}
		if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
			t.Fatalf("sync %d took the user's value: %q, %v\n%s", i, v, ok, read())
		}
	}
	setAgentsV1(t, false)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
		t.Fatalf("off overwrote the user's value: %q, %v\n%s", v, ok, read())
	}
	// a new enable captures what is there now (true), then puts it back
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, _ := feature(t, cx, "multi_agent_v2"); v != "false" {
		t.Fatalf("re-enabled: %q\n%s", v, read())
	}
	setAgentsV1(t, false)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
		t.Fatalf("off after re-enable: %q, %v\n%s", v, ok, read())
	}
}

// A key the user takes out by hand while the setting is on is left out:
// repeated syncs don't put it back and the setting off doesn't either.
func TestCodexAgentsV1ManualRemovalIsKept(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := edit.DelTOMLKey(cx.Path, "features", "multi_agent_v2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := cx.Sync(); err != nil {
			t.Fatal(err)
		}
		if v, ok := feature(t, cx, "multi_agent_v2"); ok {
			t.Fatalf("sync %d put the key back: %q\n%s", i, v, read())
		}
	}
	setAgentsV1(t, false)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); ok {
		t.Fatalf("off put the key back: %q\n%s", v, read())
	}
}

// A write the filesystem refuses keeps magpie's record of the user's value,
// so a later sync puts it back rather than losing it.
func TestCodexAgentsV1RestoreWriteFailureRetries(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(cx.Path)
	readOnly(t, dir)
	setAgentsV1(t, false)
	if err := cx.Sync(); err == nil {
		t.Fatal("the write is refused, but Sync said it was fine")
	}
	if v, _ := feature(t, cx, "multi_agent_v2"); v != "false" {
		t.Fatalf("the value moved without a write: %q\n%s", v, read())
	}
	if was := stashLoad()["codex.multiAgentV2"]; was != "true" {
		t.Fatalf("the record was dropped before the write landed: %q", was)
	}
	makeWritable(t, dir)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
		t.Fatalf("retry: %q, %v\n%s", v, ok, read())
	}
	if was := stashLoad()["codex.multiAgentV2"]; was != "" {
		t.Fatalf("the record stayed after the write landed: %q", was)
	}
}

// A write the filesystem refuses on the way on leaves no record of a change
// magpie never made, so a later sync tries again.
func TestCodexAgentsV1EnableWriteFailureLeavesNoOwnership(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(cx.Path)
	readOnly(t, dir)
	setAgentsV1(t, true)
	if err := cx.Sync(); err == nil {
		t.Fatal("the write is refused, but Sync said it was fine")
	}
	if v, _ := feature(t, cx, "multi_agent_v2"); v != "true" {
		t.Fatalf("the value moved without a write: %q\n%s", v, read())
	}
	if was := stashLoad()["codex.multiAgentV2"]; was != "" {
		t.Fatalf("a refused write left magpie claiming the key: %q", was)
	}
	makeWritable(t, dir)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, _ := feature(t, cx, "multi_agent_v2"); v != "false" {
		t.Fatalf("retry: %q\n%s", v, read())
	}
	if was := stashLoad()["codex.multiAgentV2"]; was != "true" {
		t.Fatalf("the user's value wasn't kept: %q", was)
	}
}

// A record of the user's value magpie can't write durably keeps magpie from
// applying the override at all: the config stays the user's, and the failure
// is told rather than leaving a change with nothing to undo it.
func TestCodexAgentsV1BackupWriteFailure(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	// the Codex config stays writable; only magpie's own folder, where the
	// record goes, refuses the write
	readOnly(t, filepath.Dir(stashPath()))
	if err := cx.Sync(); err == nil {
		t.Fatal("the record can't be written, but Sync said it was fine")
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
		t.Fatalf("the override was applied with no record of the value to put back: %q, %v\n%s", v, ok, read())
	}
	if was := stashLoad()["codex.multiAgentV2"]; was != "" {
		t.Fatalf("a failed record left a claim of ownership: %q", was)
	}
	if n := cx.Notice(); !strings.Contains(n, "multi-agent V1") {
		t.Fatalf("no notice that the override wasn't written: %q", n)
	}
}

// An unreadable stash is not overwritten with an empty one, and nothing is
// put back from data magpie can't read: the failure is told and the config
// stays as the user left it.
func TestCodexAgentsV1CorruptStashKept(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	const bad = "{ not the stash"
	if err := os.MkdirAll(filepath.Dir(stashPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stashPath(), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	if err := cx.Sync(); err == nil {
		t.Fatal("the stash can't be read, but Sync said it was fine")
	}
	if v, ok := feature(t, cx, "multi_agent_v2"); !ok || v != "true" {
		t.Fatalf("the override was applied over an unreadable stash: %q, %v\n%s", v, ok, read())
	}
	if b, err := os.ReadFile(stashPath()); err != nil || string(b) != bad {
		t.Fatalf("the unreadable stash was overwritten: %q, %v", b, err)
	}
}

// A config TOML's parser refuses is a read error, not a key the user doesn't
// have: magpie says so and keeps what it knows.
func TestCodexAgentsV1ConfigReadError(t *testing.T) {
	home, read := codexAgentsHome(t, "model = \"gpt-5.5\"\n\n[features]\nmulti_agent_v2 = true\n")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	setAgentsV1(t, true)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	// another tool's half-write, as a config TOML the parser refuses
	if err := os.WriteFile(cx.Path, []byte(read()+"\noops =\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err == nil {
		t.Fatal("an unreadable config, but Sync said it was fine")
	}
	if was := stashLoad()["codex.multiAgentV2"]; was != "true" {
		t.Fatalf("the record was lost on a read error: %q", was)
	}
	if !strings.Contains(read(), "multi_agent_v2 = false") {
		t.Fatalf("the config was rewritten on a read error:\n%s", read())
	}
}

// The notice says what didn't land rather than implying success: Codex not
// connected to magpie, the user's own value left in place, or a value magpie
// couldn't put back; nothing where all is as asked.
func TestCodexAgentsV1Notice(t *testing.T) {
	home, _ := codexAgentsHome(t, "model = \"gpt-5.5\"\n")
	cx := codex(home)
	setAgentsV1(t, true)
	if n := cx.Notice(); !strings.Contains(n, "multi-agent V1") || !strings.Contains(n, "isn't connected") {
		t.Fatalf("unrouted: %q", n)
	}
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if n := cx.Notice(); strings.Contains(n, "multi-agent V1") {
		t.Fatalf("applied: %q", n)
	}
	if err := edit.SetTOMLKey(cx.Path, "features", "multi_agent_v2", true); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if n := cx.Notice(); !strings.Contains(n, "multi-agent V1") || !strings.Contains(n, "left as it is") {
		t.Fatalf("manual: %q", n)
	}
	setAgentsV1(t, false)
	stash(map[string]string{"codex.multiAgentV2": "true"})
	if n := cx.Notice(); !strings.Contains(n, "couldn't put your own multi_agent_v2 back") {
		t.Fatalf("off, unwritten: %q", n)
	}
}
