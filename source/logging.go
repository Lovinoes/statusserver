package main

import (
	"bufio"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// debugEnabled is set once at startup and read on every request/tick. It is an
// atomic so there is no data race between the collector goroutine, the HTTP
// handler goroutines, and startup, regardless of platform.
var debugEnabled atomic.Bool

// setDebug toggles verbose debug logging.
func setDebug(on bool) { debugEnabled.Store(on) }

// debugMode reports whether debug logging is on.
func debugMode() bool { return debugEnabled.Load() }

// debugf logs a message only when debug mode is enabled. The "DEBUG " prefix
// makes verbose lines easy to grep or filter out.
func debugf(format string, args ...any) {
	if debugEnabled.Load() {
		log.Printf("DEBUG "+format, args...)
	}
}

// logResponseWriter wraps http.ResponseWriter to capture the status code and
// number of bytes written for the access log.
//
// It deliberately implements Unwrap so that http.ResponseController and, more
// importantly, the websocket library (coder/websocket walks Unwrap to find the
// http.Hijacker) can still reach the underlying ResponseWriter. Without this,
// /ws upgrades would fail. http.Flusher is also forwarded so streaming keeps
// working. This is all standard-library based and therefore cross-platform.
type logResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
	hijacked    bool
}

func (w *logResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		// Mirror net/http: first Write implies 200.
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap exposes the underlying ResponseWriter so features detected via type
// assertion (Hijacker, Flusher, etc.) keep working through the wrapper.
func (w *logResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack forwards to the underlying connection if it supports hijacking, which
// the websocket handshake requires. Marking hijacked lets the access log note
// the upgrade instead of a misleading status code.
func (w *logResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	c, rw, err := hj.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, rw, err
}

// Flush forwards to the underlying Flusher when present.
func (w *logResponseWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// accessLog wraps a handler and logs one line per HTTP request: method, path,
// client, status, size and duration. Always on (this is the "normal log
// window" behaviour). When debug mode is enabled it additionally logs request
// headers and query details.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		lw := &logResponseWriter{ResponseWriter: w, status: http.StatusOK}

		if debugMode() {
			debugf("--> %s %s proto=%s host=%s remote=%s ua=%q referer=%q query=%q",
				r.Method, r.URL.Path, r.Proto, r.Host, r.RemoteAddr,
				r.UserAgent(), r.Referer(), redactQuery(r.URL.RawQuery))
			for name, vals := range r.Header {
				for _, v := range vals {
					debugf("    header %s: %s", name, redactHeader(name, v))
				}
			}
		}

		next.ServeHTTP(lw, r)

		dur := time.Since(start)
		if lw.hijacked {
			// Connection was hijacked (websocket). Status/bytes are not
			// meaningful for the upgraded connection.
			log.Printf("%s %s %s [upgraded] %s",
				clientIP(r), r.Method, r.URL.Path, dur.Round(time.Microsecond))
			return
		}
		log.Printf("%s %s %s -> %d (%d bytes) %s",
			clientIP(r), r.Method, r.URL.Path, lw.status, lw.bytes, dur.Round(time.Microsecond))
	})
}

// clientIP returns a best-effort client address for logging. Unlike remoteIP
// used for rate limiting, this is informational only and always prefers the
// real transport address, falling back to RemoteAddr verbatim.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// redactHeader hides the value of sensitive headers so tokens are never
// written to logs, even in debug mode.
func redactHeader(name, value string) string {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization":
		return "[redacted]"
	default:
		return value
	}
}

// redactQuery masks sensitive query parameters (notably the auth token) before
// a request's query string is logged in debug mode.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		// Unparseable; return a placeholder rather than risk leaking a token.
		return "[unparseable]"
	}
	if _, ok := values["token"]; ok {
		values.Set("token", "[redacted]")
	}
	return values.Encode()
}
