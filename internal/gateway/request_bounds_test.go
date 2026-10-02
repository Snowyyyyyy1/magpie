package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func TestRequestBodyTooLarge(t *testing.T) {
	for _, api := range []string{"chat", "responses", "messages", "count", "gemini", "codex", "images", "edits", "videos"} {
		t.Run(api, func(t *testing.T) {
			s := New()
			r := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
			r.ContentLength = 129 << 20
			w := httptest.NewRecorder()
			switch api {
			case "chat":
				s.handle(provider.Chat)(w, r)
			case "responses":
				s.handle(provider.Responses)(w, r)
			case "messages":
				s.handle(provider.Anthropic)(w, r)
			case "count":
				s.countTokens(w, r)
			case "codex":
				s.codexBackend(w, r)
			case "images":
				s.images(false)(w, r)
			case "edits":
				s.images(true)(w, r)
			case "videos":
				s.videosCreate(w, r)
			case "gemini":
				r.SetPathValue("call", "m:generateContent")
				s.gemini(w, r)
			}
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", w.Code)
			}
		})
	}
}

func TestRequestBodyBudgetRelease(t *testing.T) {
	s := New()
	s.requestLimits = requestLimits{body: 16, budget: 8, requests: 2}
	read := func(body string) ([]byte, func(), *httptest.ResponseRecorder, bool) {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.ContentLength = -1 // chunked and compressed inputs cannot trust a length
		w := httptest.NewRecorder()
		b, release, ok := s.requestBody(w, r, provider.Chat)
		return b, release, w, ok
	}
	b, release, _, ok := read("123456")
	if !ok || string(b) != "123456" {
		t.Fatal("initial request refused")
	}
	if _, _, w, ok := read("789"); ok || w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("aggregate budget: status %d, ok %v", w.Code, ok)
	}
	release()
	release() // exactly once
	_, release, _, ok = read("12345678")
	if !ok {
		t.Fatal("failed admission retained its budget")
	}
	release()
	if _, _, w, ok := read(strings.Repeat("x", 17)); ok || w.Code != 413 {
		t.Fatalf("unknown body size: status %d", w.Code)
	}
	s.budget.mu.Lock()
	defer s.budget.mu.Unlock()
	if s.budget.requests != 0 || s.budget.bytes != 0 {
		t.Fatalf("leaked budget: %d requests, %d bytes", s.budget.requests, s.budget.bytes)
	}
}

func TestRequestAdmissionLimit(t *testing.T) {
	s := New()
	s.requestLimits.requests = 1
	first := httptest.NewRequest("POST", "/", strings.NewReader("a"))
	_, release, ok := s.requestBody(httptest.NewRecorder(), first, provider.Chat)
	if !ok {
		t.Fatal("first admission refused")
	}
	w := httptest.NewRecorder()
	if _, _, ok := s.requestBody(w, httptest.NewRequest("POST", "/", strings.NewReader("b")), provider.Chat); ok || w.Code != 503 {
		t.Fatalf("admission cap: status %d", w.Code)
	}
	release()
	_, release, ok = s.requestBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("c")), provider.Chat)
	if !ok {
		t.Fatal("released admission was not reusable")
	}
	release()
}

func TestRequestBodyReadErrorReleasesBudget(t *testing.T) {
	s := New()
	r := httptest.NewRequest("POST", "/", strings.NewReader(""))
	r.Body = io.NopCloser(io.MultiReader(strings.NewReader("abc"), failedBodyReader{}))
	w := httptest.NewRecorder()
	if _, _, ok := s.requestBody(w, r, provider.Chat); ok || w.Code != 400 {
		t.Fatalf("read failure: status %d", w.Code)
	}
	if s.budget.bytes != 0 || s.budget.requests != 0 {
		t.Fatal("read failure retained budget")
	}
}

type failedBodyReader struct{}

func (failedBodyReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRequestBodySocketTimeout(t *testing.T) {
	s := New()
	s.requestLimits.readTimeout = 40 * time.Millisecond
	gw := httptest.NewServer(s.handle(provider.Chat))
	defer gw.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(gw.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 20\r\n\r\n{")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 408 {
		t.Fatalf("slow body: status %d", res.StatusCode)
	}
	within(t, "timed out body admission released", func() bool {
		s.budget.mu.Lock()
		defer s.budget.mu.Unlock()
		return s.budget.bytes == 0 && s.budget.requests == 0
	})
}

func TestRequestBodyDeadlineCleared(t *testing.T) {
	s := New()
	s.requestLimits.readTimeout = 30 * time.Millisecond
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlines := []time.Time{}
		_, release, ok := s.requestBody(deadlineRecorder{w, &deadlines}, r, provider.Chat)
		if len(deadlines) != 2 || !deadlines[len(deadlines)-1].IsZero() {
			t.Errorf("body deadline was not cleared: %v", deadlines)
		}
		if !ok {
			return
		}
		defer release()
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		// The read deadline must be cleared before the first response byte;
		// otherwise the server closes keepalive input during a long stream.
		io.WriteString(w, "last\n")
	}))
	defer gw.Close()
	res, err := http.Post(gw.URL, "application/json", strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || string(b) != "first\nlast\n" {
		t.Fatalf("long response = %q, %v", b, err)
	}
}

func TestCodexExpandedBodyBound(t *testing.T) {
	for _, enc := range []string{"gzip", "zstd"} {
		t.Run(enc, func(t *testing.T) {
			var compressed bytes.Buffer
			if enc == "gzip" {
				zw := gzip.NewWriter(&compressed)
				zw.Write(bytes.Repeat([]byte("x"), 2048))
				zw.Close()
			} else {
				zw, err := zstd.NewWriter(&compressed)
				if err != nil {
					t.Fatal(err)
				}
				zw.Write(bytes.Repeat([]byte("x"), 2048))
				zw.Close()
			}
			s := New()
			s.requestLimits.body = 1024
			r := httptest.NewRequest("POST", CodexPath+"/responses", &compressed)
			r.Header.Set("Content-Encoding", enc)
			w := httptest.NewRecorder()
			s.codexBackend(w, r)
			if w.Code != 413 {
				t.Fatalf("inflated %s: status %d", enc, w.Code)
			}
			if s.budget.bytes != 0 || s.budget.requests != 0 {
				t.Fatal("inflated body leaked admission")
			}
		})
	}
}

func TestProviderQueueBoundsWithoutFallback(t *testing.T) {
	fresh(t)
	v := &slowVendor{release: map[string]chan struct{}{}, quit: make(chan struct{})}
	up := httptest.NewServer(v)
	defer up.Close()
	defer close(v.quit)
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		io.WriteString(w, `{}`)
	}))
	defer fallback.Close()
	one := 1
	for _, p := range []provider.Provider{
		{ID: "slow", Name: "Slow", Key: "k", Models: []string{"m"}, Chat: up.URL + "/v1", MaxConcurrency: &one},
		{ID: "fallback", Name: "Fallback", Key: "k", Models: []string{"m"}, Chat: fallback.URL + "/v1"},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "bounded", Name: "Bounded", Members: []string{"slow/m", "fallback/m"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.lanes.maxWaiting = 1
	s.lanes.waitTimeout = 150 * time.Millisecond
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()
	send := func(tag string) (*http.Response, error) {
		b := `{"model":"group/bounded","stream":true,"messages":[{"role":"user","content":"` + tag + `"}]}`
		return http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(b))
	}
	first, err := send("active")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	done := make(chan *http.Response, 1)
	go func() {
		res, err := send("waiting")
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	within(t, "one request waiting", func() bool {
		for _, lane := range s.Lanes() {
			if lane.Waiting == 1 {
				return true
			}
		}
		return false
	})
	full, err := send("overflow")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, full.Body)
	full.Body.Close()
	if full.StatusCode != 503 || full.Header.Get("Retry-After") == "" {
		t.Fatalf("full queue: status %d", full.StatusCode)
	}
	select {
	case timed := <-done:
		if timed == nil {
			t.Fatal("queue timeout returned no response")
		}
		io.Copy(io.Discard, timed.Body)
		timed.Body.Close()
		if timed.StatusCode != 503 {
			t.Fatalf("queue timeout: status %d", timed.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue did not time out")
	}
	if fallbackCalls.Load() != 0 || !slices.Equal(v.seen(), []string{"active"}) {
		t.Fatalf("queue overload changed routing: vendor %v, fallback %d", v.seen(), fallbackCalls.Load())
	}
	for _, lane := range s.Lanes() {
		if lane.Waiting != 0 || lane.Busy != 1 {
			t.Fatalf("timeout retained queue slot: %+v", lane)
		}
	}
	within(t, "queue rejections recorded as local", func() bool {
		rejected := 0
		for _, rec := range usage.Load(time.Time{}) {
			if rec.IsRejected() && rec.Status == 503 {
				rejected++
			}
		}
		return rejected == 2
	})
	close(v.gate("active"))
	b, err := io.ReadAll(first.Body)
	if err != nil || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("active stream ended at queue timeout: %q, %v", b, err)
	}
	within(t, "all request resources released", func() bool {
		s.budget.mu.Lock()
		defer s.budget.mu.Unlock()
		return s.budget.requests == 0 && s.budget.bytes == 0
	})
	within(t, "all lanes released", func() bool { return len(s.Lanes()) == 0 })
}

// Implementing Unwrap lets ResponseController use the real socket writer;
// intercept only deadline calls so the streaming test verifies the clear.
type deadlineRecorder struct {
	http.ResponseWriter
	deadlines *[]time.Time
}

func (w deadlineRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w deadlineRecorder) SetReadDeadline(at time.Time) error {
	*w.deadlines = append(*w.deadlines, at)
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(at)
}

func TestRejectedBodySocketDoesNotDrain(t *testing.T) {
	for _, overAdmission := range []bool{false, true} {
		t.Run(fmt.Sprint(overAdmission), func(t *testing.T) {
			s := New()
			want := http.StatusRequestEntityTooLarge
			s.requestLimits.body = 4
			if overAdmission {
				want = http.StatusServiceUnavailable
				s.requestLimits.body = 64
				s.requestLimits.requests = 1
				_, release, ok := s.requestBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("held")), provider.Chat)
				if !ok {
					t.Fatal("could not hold first admission")
				}
				defer release()
			}
			gw := httptest.NewServer(s.handle(provider.Chat))
			defer gw.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(gw.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 20\r\n\r\n{")
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != want {
				t.Fatalf("rejected slow input: status %d, want %d", res.StatusCode, want)
			}
		})
	}
}
