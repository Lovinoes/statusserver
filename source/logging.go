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

// debugEnabled is atomic: read by the collector goroutine and http handlers.
var debugEnabled atomic.Bool

func setDebug(on bool) { debugEnabled.Store(on) }

func debugMode() bool { return debugEnabled.Load() }

func debugf(format string, args ...any) {
	if debugEnabled.Load() {
		log.Printf("DEBUG "+format, args...)
	}
}

// logResponseWriter wraps http.ResponseWriter to grab the status code and
// bytes written for the access log. it implements Unwrap/Hijack/Flush so the
// websocket library (which needs http.Hijacker) can reach the real writer.
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
		// mirror net/http: first Write implies 200.
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

// Hijack forwards to the underlying connection, which the websocket handshake
// needs.
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

func (w *logResponseWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// accessLog logs one line per http request. in debug mode it also logs request
// headers and query details (with secrets redacted).
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
			// hijacked (websocket): status/bytes aren't meaningful.
			log.Printf("%s %s %s [upgraded] %s",
				clientIP(r), r.Method, r.URL.Path, dur.Round(time.Microsecond))
			return
		}
		log.Printf("%s %s %s -> %d (%d bytes) %s",
			clientIP(r), r.Method, r.URL.Path, lw.status, lw.bytes, dur.Round(time.Microsecond))
	})
}

// clientIP returns a best-effort client address for logging only (always the
// transport address, never a spoofable header).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// redactHeader hides the value of sensitive headers so tokens never reach logs.
func redactHeader(name, value string) string {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization":
		return "[redacted]"
	default:
		return value
	}
}

// redactQuery masks sensitive query parameters (notably the auth token).
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "[unparseable]"
	}
	if _, ok := values["token"]; ok {
		values.Set("token", "[redacted]")
	}
	return values.Encode()
}
