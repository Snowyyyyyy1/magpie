package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/steady"
)

// The stash remembers what an agent's config said before magpie pointed it
// at the gateway, so switching back restores it instead of guessing.

func stashPath() string { return filepath.Join(filepath.Dir(provider.Path()), "stash.json") }

func stashLoad() map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(stashPath()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

// stashLoadChecked is stashLoad for a writer: a file that isn't there yet is
// an empty stash, while one that can't be read or parsed is an error — an
// unreadable stash must not be overwritten with an empty one and lose what
// magpie has to put back.
func stashLoadChecked() (map[string]string, error) {
	b, err := os.ReadFile(stashPath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", stashPath(), err)
	}
	return out, nil
}

func stash(kv map[string]string) {
	_ = stashChecked(kv)
}

// forget drops stashed values without restoring them.
func forget(keys ...string) {
	_ = forgetChecked(keys...)
}

// stashChecked is stash, reporting a write that didn't land. A caller that
// must not go on without a durable record — the Codex subagents override,
// which has to be put back — takes a failure rather than leaving a change
// with nothing to undo it.
func stashChecked(kv map[string]string) error {
	m, err := stashLoadChecked()
	if err != nil {
		return err
	}
	for k, v := range kv {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return stashSave(m)
}

// forgetChecked is forget, reporting a write that didn't land.
func forgetChecked(keys ...string) error {
	m, err := stashLoadChecked()
	if err != nil {
		return err
	}
	for _, k := range keys {
		delete(m, k)
	}
	return stashSave(m)
}

// stashSave writes the whole stash through a temp file renamed over it, as
// the configs magpie edits are written, so a failure is reported and a crash
// can't leave half a file behind.
func stashSave(m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := stashPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".stash-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return steady.Rename(tmp, path)
}

func unstash(key string) string {
	m := stashLoad()
	v := m[key]
	if v != "" {
		delete(m, key)
		b, _ := json.MarshalIndent(m, "", "  ")
		os.WriteFile(stashPath(), b, 0o600)
	}
	return v
}
