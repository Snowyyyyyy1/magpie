package gateway

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// These bound the resources retained by client requests, including requests
// waiting for a provider slot. They do not set a generation or stream deadline.
const (
	defaultBodyLimit    = 128 << 20
	defaultBodyBudget   = 256 << 20
	defaultRequestLimit = 128
	defaultBodyTimeout  = 2 * time.Minute
)

type requestLimits struct {
	body, budget int64
	requests     int
	readTimeout  time.Duration
}

type requestBudget struct {
	mu       sync.Mutex
	requests int
	bytes    int64
}

// requestBody owns an admission until release, so a waiting or streaming
// handler cannot retain its body outside the aggregate budget.
func (s *Server) requestBody(w http.ResponseWriter, r *http.Request, from provider.Protocol) ([]byte, func(), bool) {
	return s.readRequestBody(w, r, from, nil)
}

func (s *Server) readRequestBody(w http.ResponseWriter, r *http.Request, from provider.Protocol, decode func(*http.Request) (io.ReadCloser, error)) ([]byte, func(), bool) {
	controller := http.NewResponseController(w)
	reject := func(status int, message string) ([]byte, func(), bool) {
		// net/http otherwise drains a short rejected body before writing the
		// reply. A stalled client must not turn rejection into an unbounded read.
		w.Header().Set("Connection", "close")
		controller.SetReadDeadline(time.Now())
		if r.Body != nil {
			r.Body.Close()
		}
		controller.SetReadDeadline(time.Time{})
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		writeError(w, from, status, message)
		return nil, nil, false
	}
	limits := s.requestLimits
	if limits.body <= 0 {
		limits.body = defaultBodyLimit
	}
	if limits.budget <= 0 {
		limits.budget = defaultBodyBudget
	}
	if limits.requests <= 0 {
		limits.requests = defaultRequestLimit
	}
	if limits.readTimeout <= 0 {
		limits.readTimeout = defaultBodyTimeout
	}
	if r.ContentLength > limits.body {
		return reject(http.StatusRequestEntityTooLarge, "request body exceeds gateway limit")
	}
	s.budget.mu.Lock()
	if s.budget.requests >= limits.requests {
		s.budget.mu.Unlock()
		return reject(http.StatusServiceUnavailable, "gateway request capacity is full")
	}
	s.budget.requests++
	s.budget.mu.Unlock()
	var held int64
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.budget.mu.Lock()
			s.budget.requests--
			s.budget.bytes -= held
			s.budget.mu.Unlock()
		})
	}
	fail := func(status int, message string) ([]byte, func(), bool) {
		release()
		return reject(status, message)
	}
	readFailure := func(err error) ([]byte, func(), bool) {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return fail(http.StatusRequestTimeout, "request body read timed out")
		}
		return fail(http.StatusBadRequest, err.Error())
	}
	if err := controller.SetReadDeadline(time.Now().Add(limits.readTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fail(http.StatusInternalServerError, "could not set request body deadline")
	}
	defer controller.SetReadDeadline(time.Time{})
	rd := r.Body
	if decode != nil {
		var err error
		rd, err = decode(r)
		if err != nil {
			return readFailure(err)
		}
		defer rd.Close()
	}
	// Fixed-size reads keep uncharged bytes bounded even with many concurrent
	// readers. Buffer capacity and growth copies are at most a small multiple
	// of the budget; no allocation trusts the client's Content-Length.
	var body bytes.Buffer
	var scratch [32 << 10]byte
	for {
		n, err := rd.Read(scratch[:])
		if n > 0 {
			if held+int64(n) > limits.body {
				return fail(http.StatusRequestEntityTooLarge, "request body exceeds gateway limit")
			}
			s.budget.mu.Lock()
			if s.budget.bytes+int64(n) > limits.budget {
				s.budget.mu.Unlock()
				return fail(http.StatusServiceUnavailable, "gateway request body capacity is full")
			}
			s.budget.bytes += int64(n)
			held += int64(n)
			s.budget.mu.Unlock()
			body.Write(scratch[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return readFailure(err)
		}
	}
	return body.Bytes(), release, true
}
