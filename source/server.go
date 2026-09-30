package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Server holds the latest snapshot and serves it over HTTP and websocket.
type Server struct {
	cfg       Config
	hub       *Hub
	tokenHash [32]byte // sha256 of cfg.AuthToken, so comparisons are fixed-length

	mu       sync.RWMutex
	snap     *Snapshot
	snapJSON []byte

	shutdown     chan struct{} // closed to tell websocket handlers to wrap up
	shutdownOnce sync.Once
}

func newServer(cfg Config) *Server {
	return &Server{
		cfg:       cfg,
		hub:       newHub(cfg.MaxConnsPerIP),
		tokenHash: sha256.Sum256([]byte(cfg.AuthToken)),
		shutdown:  make(chan struct{}),
	}
}

// publish makes snap the current snapshot everywhere: /api/status, /metrics,
// /readyz and every websocket client.
func (s *Server) publish(snap *Snapshot, data []byte) {
	s.mu.Lock()
	s.snap, s.snapJSON = snap, data
	s.mu.Unlock()
	s.hub.publish(data)
}

func (s *Server) latest() (*Snapshot, []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.snapJSON
}

// beginShutdown tells websocket handlers to send a close frame and exit.
func (s *Server) beginShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

// maxSnapshotAge is how stale the latest snapshot may get before /readyz
// reports the collector as stuck.
func (s *Server) maxSnapshotAge() time.Duration {
	return 3*time.Duration(s.cfg.IntervalSeconds)*time.Second + 30*time.Second
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("OPTIONS /api/status", s.handleStatus)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /version", handleVersion)
	return accessLog(securityHeaders(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("Vary", "Origin")
	if origin := r.Header.Get("Origin"); origin != "" && originAllowed(s.cfg.AllowedOrigins, origin) {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Headers", "Authorization")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Max-Age", "600")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.authorized(r) {
		unauthorized(w)
		return
	}
	h.Set("Cache-Control", "no-store")
	_, data := s.latest()
	if data == nil {
		h.Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no snapshot collected yet"})
		return
	}
	h.Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		unauthorized(w)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.cfg.AllowedOrigins,
	})
	if err != nil {
		// Accept has already written an error response.
		debugf("ws accept error: %v", err)
		return
	}

	ip := remoteIP(r, s.cfg.TrustProxyHeaders)
	c := newClient(conn, ip)
	if !s.hub.add(c) {
		debugf("ws rejected: ip=%s (per-IP limit %d or shutting down)", ip, s.cfg.MaxConnsPerIP)
		conn.Close(websocket.StatusTryAgainLater, "connection limit reached")
		return
	}
	defer s.hub.remove(c)
	debugf("ws connected: ip=%s clients=%d", ip, s.hub.count())
	c.serve(s.shutdown)
	debugf("ws disconnected: ip=%s", ip)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		unauthorized(w)
		return
	}
	snap, _ := s.latest()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	writeMetrics(w, snap, s.hub.count())
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz is ready once a snapshot exists, and stops being ready if the
// collector falls far behind (e.g. stuck on a hung filesystem).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	snap, _ := s.latest()
	switch {
	case snap == nil:
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	case time.Since(snap.Timestamp) > s.maxSnapshotAge():
		http.Error(w, "stale: last snapshot at "+snap.Timestamp.Format(time.RFC3339), http.StatusServiceUnavailable)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ready\n"))
	}
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": version,
		"commit":  commit,
		"date":    date,
		"go":      runtime.Version(),
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="statusserver"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// authorized checks the ?token= query parameter or an Authorization: Bearer
// header against the configured token. With no token configured everything
// is allowed.
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.AuthToken == "" {
		return true
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = bearerToken(r.Header.Get("Authorization"))
	}
	if tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(sum[:], s.tokenHash[:]) == 1
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header value; the scheme is case-insensitive per RFC 7235.
func bearerToken(header string) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// originAllowed reports whether a browser Origin matches the allow-list,
// using the same rules as the websocket handshake: a pattern containing
// "://" is matched against scheme://host, anything else against the host
// alone, both as case-insensitive path.Match globs ("*" allows any).
func originAllowed(patterns []string, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, p := range patterns {
		target := u.Host
		if strings.Contains(p, "://") {
			target = u.Scheme + "://" + u.Host
		}
		if ok, _ := path.Match(strings.ToLower(p), strings.ToLower(target)); ok {
			return true
		}
	}
	return false
}

// remoteIP returns the client IP used for per-IP limits. With trustProxy set
// it uses the last X-Forwarded-For hop - the one appended by the (single)
// trusted reverse proxy in front of us. Earlier hops are supplied by the
// client and can't be trusted. Otherwise it uses the transport address.
func remoteIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			hops := strings.Split(vals[len(vals)-1], ",")
			last := strings.TrimSpace(hops[len(hops)-1])
			if addr, err := netip.ParseAddr(last); err == nil {
				return addr.Unmap().String()
			}
			// some proxies append ip:port.
			if ap, err := netip.ParseAddrPort(last); err == nil {
				return ap.Addr().Unmap().String()
			}
		}
	}
	return transportIP(r)
}

// transportIP is the address of the directly connected peer.
func transportIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
