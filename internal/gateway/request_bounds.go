package gateway

import (
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// Bound a single body and its read time without limiting how long a client
// may wait for a provider slot or how many streams a team gateway may serve.
const (
	defaultBodyLimit   = 128 << 20
	defaultBodyTimeout = 2 * time.Minute
)

type requestLimits struct {
	body        int64
	readTimeout time.Duration
}

func (s *Server) requestBody(w http.ResponseWriter, r *http.Request, from provider.Protocol) ([]byte, bool) {
	return s.readRequestBody(w, r, from, nil)
}

func (s *Server) readRequestBody(w http.ResponseWriter, r *http.Request, from provider.Protocol, decode func(*http.Request) (io.ReadCloser, error)) ([]byte, bool) {
	controller := http.NewResponseController(w)
	reject := func(status int, message string) ([]byte, bool) {
		// net/http otherwise drains a short rejected body before writing the
		// reply. A stalled client must not turn rejection into an unbounded read.
		w.Header().Set("Connection", "close")
		controller.SetReadDeadline(time.Now())
		if r.Body != nil {
			r.Body.Close()
		}
		controller.SetReadDeadline(time.Time{})
		writeError(w, from, status, message)
		return nil, false
	}
	limits := s.requestLimits
	if limits.body <= 0 {
		limits.body = defaultBodyLimit
	}
	if limits.readTimeout <= 0 {
		limits.readTimeout = defaultBodyTimeout
	}
	if r.ContentLength > limits.body {
		return reject(http.StatusRequestEntityTooLarge, "request body exceeds gateway limit")
	}
	if err := controller.SetReadDeadline(time.Now().Add(limits.readTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return reject(http.StatusInternalServerError, "could not set request body deadline")
	}
	defer controller.SetReadDeadline(time.Time{})
	readFailure := func(err error) ([]byte, bool) {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return reject(http.StatusRequestTimeout, "request body read timed out")
		}
		return reject(http.StatusBadRequest, err.Error())
	}
	rd := r.Body
	if decode != nil {
		var err error
		rd, err = decode(r)
		if err != nil {
			return readFailure(err)
		}
		defer rd.Close()
	}
	// Limit the decoded bytes too, without allocating from Content-Length.
	body, err := io.ReadAll(io.LimitReader(rd, limits.body+1))
	if int64(len(body)) > limits.body {
		return reject(http.StatusRequestEntityTooLarge, "request body exceeds gateway limit")
	}
	if err != nil {
		return readFailure(err)
	}
	return body, true
}
