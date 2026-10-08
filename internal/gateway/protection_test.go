package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A long Codex conversation's compaction, as Codex sends it: the history
// with sealed reasoning and calls, then the trigger.
const protectedCompaction = `{"model":%q,"stream":true,"store":false,"input":[` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor the parser"}]},` +
	`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAAABo-sealed"},` +
	`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
	`{"type":"function_call_output","call_id":"call_1","output":"main.go"},` +
	`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},` +
	`{"type":"compaction_trigger"}]}`

// protectionUp stands in for the ChatGPT backend answering a structured
// history 502 "response protection is unavailable" on every account, and
// the history as plain text with a summary. It notes the accounts asked.
func protectionUp(t *testing.T, said string) *[]string {
	t.Helper()
	var mu sync.Mutex
	tried := &[]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*tried = append(*tried, r.Header.Get("chatgpt-account-id"))
		mu.Unlock()
		if !strings.Contains(string(b), "This is the conversation so far") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, said)
			return
		}
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"the summary"}]}}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":7,"output_tokens":2}}}`))
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	return tried
}

func restingNow(t *testing.T) []string {
	t.Helper()
	restingUntil.Lock()
	defer restingUntil.Unlock()
	var out []string
	for k := range restingUntil.m {
		out = append(out, k)
	}
	return out
}

// vs on Discord (0.1.1108): a long Codex conversation's compaction on
// Codex's own model, three ChatGPT accounts on, the backend answering 502
// "response protection is unavailable" for that history. It is the
// request, not an account: asked once, not on every account and then the
// last again, and no account rests for it.
func TestProtectionRefusalAskedOnceNobodyRests(t *testing.T) {
	for _, said := range []string{
		`{"error":{"message":"response protection is unavailable","type":"server_error"}}`,
		`{"detail":"response protection is unavailable"}`,
	} {
		t.Run(said, func(t *testing.T) {
			codexSignedIn(t, "two@example.com", "three@example.com")
			tried := protectionUp(t, said)
			code, body := codexPost(t, strings.Replace(protectedCompaction, "%q", `"gpt-5.5"`, 1))
			if code != http.StatusBadGateway || !strings.Contains(body, "response protection is unavailable") {
				t.Fatalf("status %d: %s", code, body)
			}
			if len(*tried) != 1 {
				t.Errorf("asked %d times (%v), not once", len(*tried), *tried)
			}
			if r := restingNow(t); len(r) > 0 {
				t.Errorf("resting after a refusal of the request: %v", r)
			}
		})
	}
}

// The same compaction on a magpie model served by the ChatGPT accounts:
// refused so, it is asked again once with the conversation as plain text,
// as vs's control experiment was answered, and Codex gets its compaction.
func TestProtectionRefusedCompactionAsText(t *testing.T) {
	codexSignedIn(t, "two@example.com")
	tried := protectionUp(t, `{"error":{"message":"response protection is unavailable","type":"server_error"}}`)
	code, body := codexPost(t, strings.Replace(protectedCompaction, "%q", `"codex/gpt-5.5"`, 1))
	if code != 200 || !strings.Contains(body, `"type":"compaction"`) || !strings.Contains(body, magpieCompaction) {
		t.Fatalf("status %d: %s", code, body)
	}
	if len(*tried) != 2 {
		t.Errorf("asked %d times (%v), not once as it came and once as text", len(*tried), *tried)
	}
	if r := restingNow(t); len(r) > 0 {
		t.Errorf("resting: %v", r)
	}
}

func TestProtectionBreakOffDoesNotRest(t *testing.T) {
	if lateRests("response protection is unavailable") {
		t.Error("a reply broken off with the backend's refusal of the request rests its account")
	}
	if !lateRests("Unable to reach the model provider") {
		t.Error("a vendor failing mid-reply no longer rests")
	}
}
