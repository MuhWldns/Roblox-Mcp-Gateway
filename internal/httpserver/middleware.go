package httpserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// RequestIDHeader names the correlation header set on every response. A safe
// incoming value is echoed back so clients and servers share one correlation
// id; unsafe values are replaced.
const RequestIDHeader = "X-Request-ID"

// DefaultMaxBodyBytes bounds /api/ request bodies when the configuration
// leaves the limit unset.
const DefaultMaxBodyBytes int64 = 1 << 20

// Fixed security response headers applied to every route, including the
// SPA, the API, health probes, and the well-known discovery documents.
const (
	hstsHeaderValue           = "max-age=31536000; includeSubDomains"
	cspHeaderValue            = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'"
	contentTypeOptionsValue   = "nosniff"
	frameOptionsValue         = "DENY"
	referrerPolicyValue       = "no-referrer"
	requestIDPrefix           = "req_"
	requestIDEntropyBytes     = 16
	minIncomingRequestIDRunes = 8
	maxIncomingRequestIDRunes = 128
)

type requestIDKeyType struct{}

var requestIDKey requestIDKeyType

// newRequestID mints a fresh opaque correlation id.
func newRequestID() string {
	buf := make([]byte, requestIDEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to a
		// time-derived value so a response still carries a correlation id.
		return fmt.Sprintf("%s%d", requestIDPrefix, time.Now().UnixNano())
	}
	return requestIDPrefix + hex.EncodeToString(buf)
}

// acceptableRequestID reports whether an incoming correlation id is safe to
// echo: bounded length and a restricted charset, so a client cannot inject
// log-forging control characters through the header.
func acceptableRequestID(id string) bool {
	if len(id) < minIncomingRequestIDRunes || len(id) > maxIncomingRequestIDRunes {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

// requestID assigns the correlation id, echoes it on the response, and
// propagates it through the request context so handlers can correlate audit
// events with the observed request.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
		if !acceptableRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// requestIDFromContext returns the request's correlation id, "" when absent.
func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// wroteHeaderTracker records whether the wrapped handler committed a
// response, so panic recovery knows whether a sanitized 500 is still
// writable.
type wroteHeaderTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *wroteHeaderTracker) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *wroteHeaderTracker) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

// Hijack forwards connection hijacking to the wrapped writer when it
// supports hijacking, so WebSocket endpoints mounted behind panic
// recovery upgrade normally instead of failing with 501. A hijacked
// connection no longer speaks HTTP: mark it written so recovery never
// attempts a second status line on it.
func (t *wroteHeaderTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpserver: response writer does not support hijacking")
	}
	t.wrote = true
	return hijacker.Hijack()
}

// RecoverPanics converts a handler panic into a sanitized 500 response. The
// panic value and stack trace reach the server log only; the response never
// carries internal detail, and a mid-write panic cannot emit a second
// status line.
func RecoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker := &wroteHeaderTracker{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("http server: recovered panic: %v\n%s", recovered, debug.Stack())
				if !tracker.wrote {
					writeAPIError(w, http.StatusInternalServerError, "internal server error")
				}
			}
		}()
		next.ServeHTTP(tracker, r)
	})
}

// secureHeaders applies the fixed security header set to every response.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Strict-Transport-Security", hstsHeaderValue)
		headers.Set("Content-Security-Policy", cspHeaderValue)
		headers.Set("X-Content-Type-Options", contentTypeOptionsValue)
		headers.Set("X-Frame-Options", frameOptionsValue)
		headers.Set("Referrer-Policy", referrerPolicyValue)
		next.ServeHTTP(w, r)
	})
}

// limitBody bounds browser API request bodies. A declared Content-Length
// beyond the limit is rejected up front; everything else is capped through
// http.MaxBytesReader so chunked or lying requests fail during the read
// with an error the JSON decoders map to 413.
func limitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if max > 0 {
				if r.ContentLength > max {
					writeAPIError(w, http.StatusRequestEntityTooLarge, "request body too large")
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}
