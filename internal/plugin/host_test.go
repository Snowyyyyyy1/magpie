package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost is a host whose stdout the test writes, so its real read and
// dispatch run without Bun. Its aborts are left queued for the test unless
// it starts abortLoop.
type fakeHost struct {
	*host
	pw *io.PipeWriter
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	pr, pw := io.Pipe()
	h := &host{cmd: &exec.Cmd{}, in: discardCloser{}, calls: map[int64]*call{}, dead: make(chan struct{}), aborts: make(chan int64, 64)}
	go h.read(pr)
	t.Cleanup(func() { _ = pw.CloseWithError(io.EOF) })
	return &fakeHost{host: h, pw: pw}
}

// line is one message the host would have written on its stdout.
func (f *fakeHost) line(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	_, _ = f.pw.Write(append(b, '\n'))
}

func (f *fakeHost) head(id int64) {
	f.line(map[string]any{"id": id, "event": "head", "status": 200, "headers": map[string]string{}})
}

func (f *fakeHost) chunk(id int64, data string) {
	f.line(map[string]any{"id": id, "event": "chunk", "data": base64.StdEncoding.EncodeToString([]byte(data))})
}

func (f *fakeHost) answer(id int64, why string) {
	if why == "" {
		f.line(map[string]any{"id": id, "result": nil})
		return
	}
	f.line(map[string]any{"id": id, "error": map[string]string{"message": why}})
}

// next is the next message queued for the call, as Fetch takes its head.
func (f *fakeHost) next(t *testing.T, c *call) message {
	t.Helper()
	for {
		if m, ok := c.pop(); ok {
			return m
		}
		select {
		case <-c.wake:
		case <-time.After(5 * time.Second):
			t.Fatal("no message arrived")
		}
	}
}

// reply writes a stream's head and waits for the test to take it, as Fetch
// takes its head, then writes the rest of the reply in the background.
func reply(t *testing.T, f *fakeHost, id int64, c *call, rest func()) message {
	t.Helper()
	popped := make(chan struct{})
	go func() {
		f.head(id)
		<-popped
		rest()
	}()
	h := f.next(t, c)
	close(popped)
	return h
}

// readBody is the reply body Fetch hands out for the call.
func readBody(f *fakeHost, id int64, c *call, ctx context.Context) *body {
	return &body{h: f.host, id: id, c: c, ctx: ctx, closed: make(chan struct{})}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s didn't happen", what)
}

// registered is whether the call is still the host's to answer.
func (f *fakeHost) registered(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.calls[id]
	return ok
}

// queued is how many chunks the call holds for its reader.
func (c *call) queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// abortsTaken are the aborts the host queued for the child, without waiting:
// abortLoop is not started in these tests.
func (f *fakeHost) abortsTaken() []int64 {
	var out []int64
	for {
		select {
		case id := <-f.aborts:
			out = append(out, id)
		default:
			return out
		}
	}
}

// parked is a read left waiting for a chunk, with its error to come.
func parked(b *body) chan error {
	errs := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 64))
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the read get there
	return errs
}

// parkedCtx is a context whose Done is taken as the signal that a select has
// begun waiting on it: headRead takes Done once, entering its select, so a
// test knows the head read is waiting there without sleeping for it.
type parkedCtx struct {
	context.Context
	parked chan struct{}
	once   sync.Once
}

func (c *parkedCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.parked) })
	return c.Context.Done()
}

// TestHostStalledStreamDoesNotBlockOtherCalls is the regression this file is
// for: one stream whose consumer reads nothing must not hold up the host's
// reader, so another call's answer still comes, and the stalled stream still
// has every chunk, in order, when its consumer comes back.
func TestHostStalledStreamDoesNotBlockOtherCalls(t *testing.T) {
	f := newFakeHost(t)
	stalled, c := f.begin(true)
	other, oc := f.begin(false)
	b := readBody(f, stalled, c, context.Background())

	if h := reply(t, f, stalled, c, func() {
		for i := range 200 {
			f.chunk(stalled, chunkAt(i))
		}
		f.answer(other, "")
		f.answer(stalled, "")
	}); h.Status != 200 {
		t.Fatalf("head = %+v", h)
	}
	select {
	case m := <-oc.done:
		if m.Error != nil {
			t.Fatalf("the other call failed: %s", m.Error.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled stream blocked another call's answer")
	}

	got, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("reading the stalled stream: %v", err)
	}
	if want := chunks(200); string(got) != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
	if f.registered(stalled) {
		t.Fatal("an answered call is still the host's")
	}
}

// TestHostBodyCancelEndsAStalledRead: ctx ending must end a read waiting on
// the plugin, and give the call up with it.
func TestHostBodyCancelEndsAStalledRead(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	ctx, cancel := context.WithCancel(context.Background())
	b := readBody(f, id, c, ctx)
	reply(t, f, id, c, func() {}) // the head: no chunks ever come

	errs := parked(b)
	select {
	case err := <-errs:
		t.Fatalf("the read ended before cancel: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read = %v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel didn't end a read waiting on the plugin")
	}
	if f.registered(id) {
		t.Fatal("the call is still the host's after cancel")
	}
	if got := <-f.aborts; got != id {
		t.Fatalf("aborted %d, want %d", got, id)
	}
}

// TestHostBodyCloseEndsAnIdleRead: closing a body nothing was ever queued for
// must end a read parked on it, and give the call up.
func TestHostBodyCloseEndsAnIdleRead(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {}) // the head: nothing else ever comes

	errs := parked(b)
	select {
	case err := <-errs:
		t.Fatalf("the read ended before close: %v", err)
	default:
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, errBodyClosed) {
			t.Fatalf("a read parked at close = %v, want errBodyClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body didn't end a read parked on it")
	}
	if f.registered(id) {
		t.Fatal("the call is still the host's after its body closed")
	}
	if got := <-f.aborts; got != id {
		t.Fatalf("aborted %d, want %d", got, id)
	}
	if _, err := b.Read(make([]byte, 8)); !errors.Is(err, errBodyClosed) {
		t.Fatalf("a read after close = %v, want errBodyClosed", err)
	}
}

// TestHostStreamBacklogGivesUpOnThatRequestOnly: a consumer that never reads
// has its request given up on there and then — the call is dropped, its
// reader is told which limit it passed, and another call carries on.
func TestHostStreamBacklogGivesUpOnThatRequestOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events int
		bytes  int
		fits   int // how many of the reply's chunks are read before the error
		want   error
	}{
		{"too many chunks", 2, 4 << 20, 2, errQueuedEvents},
		{"too many bytes", 4096, 8, 3, errQueuedBytes}, // 4 base64 bytes a chunk
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldEvents, oldBytes := maxQueuedEvents, maxQueuedBytes
			maxQueuedEvents, maxQueuedBytes = tc.events, tc.bytes
			t.Cleanup(func() { maxQueuedEvents, maxQueuedBytes = oldEvents, oldBytes })

			f := newFakeHost(t)
			bad, bc := f.begin(true)
			other, oc := f.begin(false)
			b := readBody(f, bad, bc, context.Background())
			reply(t, f, bad, bc, func() {
				for i := range 20 {
					f.chunk(bad, chunkAt(i))
				}
				f.chunk(other, "never queued") // a plain call has no chunks
				f.answer(other, "")
				f.answer(bad, "")
			})
			waitFor(t, "the given-up call was dropped", func() bool { return !f.registered(bad) })
			if m, ok := oc.pop(); ok {
				t.Fatalf("a plain call queued a chunk: %+v", m)
			}
			select {
			case <-oc.done:
			case <-time.After(5 * time.Second):
				t.Fatal("giving up on one request held up another's answer")
			}

			// the error reaches the consumer, and says which limit it passed
			got, err := io.ReadAll(b)
			if !errors.Is(err, tc.want) {
				t.Fatalf("read = %v, want %v", err, tc.want)
			}
			if want := chunks(tc.fits); string(got) != want {
				t.Fatalf("stream = %q, want the %d chunks that fitted %q", got, tc.fits, want)
			}
		})
	}
}

// TestHostStreamKeepsChunkOrderAndAnswer: a stream its consumer keeps up with
// has every chunk, in order, then a clean end.
func TestHostStreamKeepsChunkOrderAndAnswer(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	if h := reply(t, f, id, c, func() {
		for i := range 50 {
			f.chunk(id, chunkAt(i))
		}
		f.answer(id, "")
	}); h.Status != 200 {
		t.Fatalf("head = %+v", h)
	}
	got, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if want := chunks(50); string(got) != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
	if f.registered(id) {
		t.Fatal("an answered call is still the host's")
	}
}

// TestHostStreamTakesOneChunkBiggerThanTheLimit: the bound is the queued
// backlog, so a reply the plugin sends whole (one chunk bigger than the byte
// limit) is still delivered rather than given up on.
func TestHostStreamTakesOneChunkBiggerThanTheLimit(t *testing.T) {
	oldBytes := maxQueuedBytes
	maxQueuedBytes = 8
	t.Cleanup(func() { maxQueuedBytes = oldBytes })

	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	whole := strings.Repeat("x", 64)
	reply(t, f, id, c, func() {
		f.chunk(id, whole)
		f.answer(id, "")
	})
	got, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("reading a whole reply: %v", err)
	}
	if string(got) != whole {
		t.Fatalf("stream = %q, want %q", got, whole)
	}
}

// TestHostStreamErrorAfterChunksReachesBody: the chunks before an answer that
// failed are still read, in order, then the error.
func TestHostStreamErrorAfterChunksReachesBody(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {
		f.chunk(id, "one")
		f.chunk(id, "two")
		f.answer(id, "the vendor exploded")
	})
	got, err := io.ReadAll(b)
	if err == nil || !strings.Contains(err.Error(), "the vendor exploded") {
		t.Fatalf("error = %v, want the plugin's", err)
	}
	if string(got) != "onetwo" {
		t.Fatalf("stream = %q, want %q", got, "onetwo")
	}
}

// TestHostShutdownEndsAStalledRead: the host quitting answers its pending
// call with why, so a read waiting on it ends rather than waiting for ever.
func TestHostShutdownEndsAStalledRead(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {})
	errs := parked(b)
	_ = f.pw.CloseWithError(io.EOF) // the child's stdout ends: it quit
	select {
	case err := <-errs:
		if err == nil || !strings.Contains(err.Error(), "plugin host quit") {
			t.Fatalf("read = %v, want the host's", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host quitting didn't end a read waiting on it")
	}
}

// blockingIn is a child's stdin that never takes a write.
type blockingIn struct{ release chan struct{} }

func (b blockingIn) Write(p []byte) (int, error) { <-b.release; return len(p), nil }
func (b blockingIn) Close() error                { return nil }

// TestHostBodyCloseIsNotHeldByAWedgedStdin: a child that stopped reading its
// stdin must not hold a body's close, nor its cleanup.
func TestHostBodyCloseIsNotHeldByAWedgedStdin(t *testing.T) {
	f := newFakeHost(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.in = blockingIn{release: release}
	go f.abortLoop() // the one goroutine that writes aborts, parked in it here

	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = b.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body waited on the child's stdin")
	}
	if f.registered(id) {
		t.Fatal("the call is still the host's after its body closed")
	}
}

// chunkAt is the i-th chunk of a stream, "" apart from its place.
func chunkAt(i int) string { return strconv.Itoa(i) + "\n" }

// TestHostHeadKeepsTheAnswerWhenItCameWithTheHead: a head read can find the
// call's answer already there and take the head out of the queue with it. The
// answer must reach the body all the same, an error staying an error.
func TestHostHeadKeepsTheAnswerWhenItCameWithTheHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  string
	}{
		{"answered", ""},
		{"failed", "the vendor exploded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHost(t)
			id, c := f.begin(true)
			type read struct {
				head   message
				answer *message
				err    error
			}
			ctx := &parkedCtx{Context: context.Background(), parked: make(chan struct{})}
			out := make(chan read, 1)
			go func() {
				h, a, err := c.headRead(ctx)
				out <- read{h, a, err}
			}()
			<-ctx.parked // the head read is waiting in its select now
			// the window the race makes: the answer is there, and a head is
			// queued with no wake for it
			c.mu.Lock()
			c.queue = append(c.queue, message{Status: 200})
			c.mu.Unlock()
			c.closed.Store(true)
			if tc.why == "" {
				c.done <- message{}
			} else {
				c.done <- errAnswer(tc.why)
			}
			r := <-out
			if r.err != nil || r.head.Status != 200 {
				t.Fatalf("head = %+v, %v", r.head, r.err)
			}
			if r.answer == nil {
				t.Fatal("the answer was lost: the body would never end")
			}
			b := &body{h: f.host, id: id, c: c, ctx: ctx, answer: r.answer, closed: make(chan struct{})}
			got, err := io.ReadAll(b)
			if len(got) != 0 {
				t.Fatalf("body = %q, want nothing", got)
			}
			switch {
			case tc.why == "":
				if err != nil {
					t.Fatalf("body ended with %v, want a clean end", err)
				}
			case err == nil || !strings.Contains(err.Error(), tc.why):
				t.Fatalf("body ended with %v, want the plugin's %q", err, tc.why)
			}
		})
	}
}

// TestHostBodyReadStopsAtCloseWithBufferedData: what is left of a chunk, and
// what is queued behind it, is not read after the body is closed.
func TestHostBodyReadStopsAtCloseWithBufferedData(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {
		f.chunk(id, "first")
		f.chunk(id, "second")
		f.answer(id, "")
	})
	if n, err := b.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("read = %d, %v, want one byte", n, err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n, err := b.Read(make([]byte, 16)); n != 0 || !errors.Is(err, errBodyClosed) {
		t.Fatalf("read after close = %d, %v, want 0, errBodyClosed", n, err)
	}
}

// TestHostBodyReadStopsAtCancelWithABacklog: a cancel is not left behind what
// is already queued.
func TestHostBodyReadStopsAtCancelWithABacklog(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	ctx, cancel := context.WithCancel(context.Background())
	b := readBody(f, id, c, ctx)
	reply(t, f, id, c, func() {
		for i := range 5 {
			f.chunk(id, chunkAt(i))
		}
		f.answer(id, "")
	})
	waitFor(t, "the chunks were queued", func() bool { return c.queued() == 5 })
	cancel()
	if n, err := b.Read(make([]byte, 64)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %d, %v, want 0, canceled", n, err)
	}
	if f.registered(id) {
		t.Fatal("the call is still the host's after cancel")
	}
}

// TestHostPushAfterCloseIsNotQueued: a chunk the reader is already gone for is
// not queued again, whatever dispatch still holds.
func TestHostPushAfterCloseIsNotQueued(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {})
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if c.push(message{Data: base64.StdEncoding.EncodeToString([]byte("hi"))}) {
		t.Fatal("a chunk was queued for a body already given up on")
	}
	if n := c.queued(); n != 0 {
		t.Fatalf("%d chunks queued for a closed body", n)
	}
}

// TestHostBodyZeroLengthRead: a read that asks for nothing reads nothing, and
// takes nothing from the call.
func TestHostBodyZeroLengthRead(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {
		f.chunk(id, "hi")
		f.answer(id, "")
	})
	waitFor(t, "the chunk was queued", func() bool { return c.queued() == 1 })
	if n, err := b.Read(nil); n != 0 || err != nil {
		t.Fatalf("zero-length read = %d, %v, want 0, nil", n, err)
	}
	if n := c.queued(); n != 1 {
		t.Fatalf("a zero-length read took a chunk: %d left", n)
	}
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "hi" {
		t.Fatalf("body = %q, %v, want hi", got, err)
	}
}

// TestHostBodyCloseDuringRead: a close under a read ends it, and leaves no
// race on what the read holds.
func TestHostBodyCloseDuringRead(t *testing.T) {
	f := newFakeHost(t)
	id, c := f.begin(true)
	b := readBody(f, id, c, context.Background())
	reply(t, f, id, c, func() {
		for i := range 50 {
			f.chunk(id, chunkAt(i))
		}
		f.answer(id, "")
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 8)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	_ = b.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read didn't end after the body closed")
	}
}

// errAnswer is the answer a plugin's fetch that failed gives.
func errAnswer(why string) message {
	return message{Error: &struct {
		Message string `json:"message"`
	}{why}}
}

// TestHostAbortIsQueuedOncePerCall: whoever takes the call out of the host's
// hands tells the child, so a call is aborted once however many of its paths
// get there — and an answered one not at all.
func TestHostAbortIsQueuedOncePerCall(t *testing.T) {
	t.Run("overflow then read and close", func(t *testing.T) {
		oldEvents := maxQueuedEvents
		maxQueuedEvents = 2
		t.Cleanup(func() { maxQueuedEvents = oldEvents })

		f := newFakeHost(t)
		id, c := f.begin(true)
		b := readBody(f, id, c, context.Background())
		reply(t, f, id, c, func() {
			for i := range 20 {
				f.chunk(id, chunkAt(i))
			}
			f.answer(id, "")
		})
		waitFor(t, "the given-up call was dropped", func() bool { return !f.registered(id) })
		if _, err := io.ReadAll(b); !errors.Is(err, errQueuedEvents) {
			t.Fatalf("read = %v, want the chunks limit", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := f.abortsTaken(); len(got) != 1 || got[0] != id {
			t.Fatalf("aborts = %v, want just %d", got, id)
		}
	})

	t.Run("answered then close", func(t *testing.T) {
		f := newFakeHost(t)
		id, c := f.begin(true)
		b := readBody(f, id, c, context.Background())
		reply(t, f, id, c, func() {
			f.chunk(id, "hi")
			f.answer(id, "")
		})
		if got, err := io.ReadAll(b); err != nil || string(got) != "hi" {
			t.Fatalf("body = %q, %v", got, err)
		}
		if f.registered(id) {
			t.Fatal("an answered call is still the host's")
		}
		if err := b.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := f.abortsTaken(); len(got) != 0 {
			t.Fatalf("aborts = %v, want none for an answered call", got)
		}
	})

	t.Run("cancel then close", func(t *testing.T) {
		f := newFakeHost(t)
		id, c := f.begin(true)
		ctx, cancel := context.WithCancel(context.Background())
		b := readBody(f, id, c, ctx)
		reply(t, f, id, c, func() {})
		cancel()
		if _, err := b.Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
			t.Fatalf("read = %v, want canceled", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := f.abortsTaken(); len(got) != 1 || got[0] != id {
			t.Fatalf("aborts = %v, want just %d", got, id)
		}
	})
}

// chunks is the first n chunks of a stream, as one body.
func chunks(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(chunkAt(i))
	}
	return b.String()
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }
