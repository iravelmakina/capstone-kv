package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID carries one request's ID from the client through the coordinator to every
// replica it calls, so its log lines can be found together (ADR-0029).
const HeaderRequestID = "X-Request-ID"

// RequestID returns the request's ID from its header, or a new random one if it has none
// or one that is too long to be an ID.
func RequestID(r *http.Request) string {
	if id := r.Header.Get(HeaderRequestID); id != "" && len(id) <= 64 {
		return id
	}
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// StatusRecorder remembers the status a handler wrote, for metrics and logs.
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

// WriteHeader records the status and passes it on.
func (s *StatusRecorder) WriteHeader(code int) {
	if s.Status == 0 {
		s.Status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write records 200 if the handler wrote a body without a status.
func (s *StatusRecorder) Write(b []byte) (int, error) {
	if s.Status == 0 {
		s.Status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Code is the status written, 200 if the handler wrote nothing.
func (s *StatusRecorder) Code() int {
	if s.Status == 0 {
		return http.StatusOK
	}
	return s.Status
}

// Flush lets a long-poll answer be sent before the handler returns.
func (s *StatusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
