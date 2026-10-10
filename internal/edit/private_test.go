package edit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A file that holds a secret is the user's alone: made 0600, and one an
// older magpie left 0644 (library.json, sync-state.json) narrowed on its
// next write, where WriteAtomic keeps the mode it finds.
func TestWritePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix modes")
	}
	d := t.TempDir()
	mode := func(p string) os.FileMode {
		t.Helper()
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}
	made := filepath.Join(d, "sync.json")
	if err := WritePrivate(made, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if m := mode(made); m != 0o600 {
		t.Errorf("new file: mode %v, want 0600", m)
	}
	old := filepath.Join(d, "library.json")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(old, []byte("{\"a\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if m := mode(old); m != 0o644 {
		t.Fatalf("WriteAtomic: mode %v, want 0644 kept", m)
	}
	if err := WritePrivate(old, []byte("{\"a\":2}\n")); err != nil {
		t.Fatal(err)
	}
	if m := mode(old); m != 0o600 {
		t.Errorf("0644 file: mode %v, want 0600", m)
	}
	if got := read(t, old); got != "{\"a\":2}\n" {
		t.Errorf("text %q", got)
	}
	// a hard-linked one is written in place, and narrowed too
	other := filepath.Join(d, "other.json")
	if err := os.WriteFile(other, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(d, "linked.json")
	hardLink(t, other, linked)
	if err := WritePrivate(linked, []byte("{\"b\":1}\n")); err != nil {
		t.Fatal(err)
	}
	sameFile(t, linked, other)
	if m := mode(linked); m != 0o600 {
		t.Errorf("hard link: mode %v, want 0600", m)
	}
}
