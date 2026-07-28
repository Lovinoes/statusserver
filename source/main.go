package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// build info, injected at link time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func versionString() string {
	return fmt.Sprintf("statusserver %s (commit %s, built %s, %s/%s, %s)",
		version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// hub tracks websocket clients and caps connections per IP (optional).
type Hub struct {
	mu            sync.Mutex
	clients       map[*websocket.Conn]string // conn -> remote ip
	perIP         map[string]int             // remote ip -> live count
	maxConnsPerIP int
}

func newHub(maxConnsPerIP int) *Hub {
	return &Hub{
		clients:       make(map[*websocket.Conn]string),
		perIP:         make(map[string]int),
		maxConnsPerIP: maxConnsPerIP,
	}
}

// tryAdd registers a client unless it would blow past the per-IP cap.
func (h *Hub) tryAdd(c *websocket.Conn, ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxConnsPerIP > 0 && h.perIP[ip] >= h.maxConnsPerIP {
		return false
	}
	h.clients[c] = ip
	h.perIP[ip]++
	return true
}

func (h *Hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	if ip, ok := h.clients[c]; ok {
		delete(h.clients, c)
		if h.perIP[ip] <= 1 {
			delete(h.perIP, ip)
		} else {
			h.perIP[ip]--
		}
	}
	h.mu.Unlock()
}

func (h *Hub) broadcast(data []byte) {
	// grab the client list under lock, then write outside it and in parallel
	// so one slow client can't hold up delivery to everyone else.
	h.mu.Lock()
	clients := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.CloseNow()
				h.remove(c)
			}
		}(c)
	}
	wg.Wait()
}

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	clients := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.clients = make(map[*websocket.Conn]string)
	h.perIP = make(map[string]int)
	h.mu.Unlock()

	for _, c := range clients {
		c.Close(websocket.StatusGoingAway, "server shutting down")
	}
}

var (
	latest        atomic.Value // most recent snapshot as marshalled json
	snapshotStore atomic.Value // most recent typed *Snapshot, for /metrics
	ready         atomic.Bool  // flips true after the first snapshot
)

func checkAuth(cfg Config, r *http.Request) bool {
	if cfg.AuthToken == "" {
		return true
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.AuthToken)) == 1
}

func corsAllowed(cfg Config, origin string) bool {
	for _, o := range cfg.AllowedOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

// warnInsecureExposure warns when the agent is reachable off-host without an
// auth token, which leaves the status/metrics endpoints wide open.
func warnInsecureExposure(cfg Config) {
	if cfg.AuthToken != "" {
		return
	}
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		host = cfg.ListenAddr
	}
	loopback := host == "127.0.0.1" || host == "::1" || host == "localhost"
	if !loopback {
		log.Printf("WARNING: auth_token is empty and listen_addr %q is not loopback; /api/status, /ws and /metrics are exposed without authentication", cfg.ListenAddr)
	}
}

// hardenedTLSConfig is the profile used when serving TLS directly: TLS 1.3
// only, key exchange locked to a post-quantum hybrid then classical curves
// (in preference order), and ALPN offering h2 with an http/1.1 fallback that
// websocket upgrades negotiate down to.
func hardenedTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{
			tls.X25519MLKEM768,
			tls.X25519,
			tls.CurveP384,
		},
		NextProtos: []string{"h2", "http/1.1"},
	}
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit (for container HEALTHCHECK)")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	if *healthcheck {
		os.Exit(runHealthcheck(loadConfig(*configPath)))
	}

	log.Printf("starting %s", versionString())

	cfg := loadConfig(*configPath)
	setDebug(cfg.Debug)
	if cfg.Debug {
		log.Println("debug logging enabled")
		debugf("effective config: %s", cfg.redactedString())
	}
	warnInsecureExposure(cfg)
	hub := newHub(cfg.MaxConnsPerIP)
	latest.Store([]byte(`{}`))

	uptime := loadUptimeStore(cfg.UptimeFile)
	collector := NewCollector(uptime)
	notifier := NewNotifier(cfg.Alerts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go collectLoop(ctx, cfg, hub, collector, notifier)

	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && corsAllowed(cfg, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !checkAuth(cfg, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(latest.Load().([]byte)); err != nil {
			log.Println("status write error:", err)
		}
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ready\n"))
	})

	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version": version,
			"commit":  commit,
			"date":    date,
			"go":      runtime.Version(),
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(cfg, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, latestSnapshot(), hub.count())
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(cfg, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: cfg.AllowedOrigins,
		})
		if err != nil {
			log.Println("ws accept error:", err)
			return
		}

		ip := remoteIP(r, cfg.TrustProxyHeaders)
		if !hub.tryAdd(c, ip) {
			debugf("ws rejected: per-IP cap reached for %s (limit %d)", ip, cfg.MaxConnsPerIP)
			c.Close(websocket.StatusTryAgainLater, "connection limit reached")
			return
		}
		defer hub.remove(c)
		debugf("ws connected: ip=%s clients=%d", ip, hub.count())

		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.Write(writeCtx, websocket.MessageText, latest.Load().([]byte))
		cancel()

		// CloseRead handles incoming control frames and cancels when the peer
		// goes away; tied to the root context so shutdown unblocks it.
		connCtx := c.CloseRead(ctx)

		// keepalive pings so dead tcp connections get spotted and reaped.
		ka := time.NewTicker(30 * time.Second)
		defer ka.Stop()
		for {
			select {
			case <-connCtx.Done():
				debugf("ws disconnected: ip=%s clients=%d", ip, hub.count()-1)
				return
			case <-ka.C:
				pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
				err := c.Ping(pingCtx)
				cancelPing()
				if err != nil {
					c.CloseNow()
					return
				}
			}
		}
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           accessLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// no overall WriteTimeout: it would kill long-lived /ws connections.
	}

	// validate the key pair up front so a bad path fails fast instead of
	// blowing up deep inside ListenAndServeTLS after we've logged "listening".
	if cfg.TLSEnabled() {
		if _, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			log.Fatalf("failed to load TLS key pair (cert %q, key %q): %v", cfg.TLSCert, cfg.TLSKey, err)
		}
		srv.TLSConfig = hardenedTLSConfig()
	}

	serverErr := make(chan error, 1)
	go func() {
		if cfg.TLSEnabled() {
			log.Printf("status server listening on %s (https, interval %ds)", cfg.ListenAddr, cfg.IntervalSeconds)
			log.Printf("TLS profile: TLS 1.3 only, key exchange X25519MLKEM768/X25519/secp384r1, ALPN h2,http/1.1")
			serverErr <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			log.Printf("status server listening on %s (http, interval %ds)", cfg.ListenAddr, cfg.IntervalSeconds)
			serverErr <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		log.Println("shutdown signal received, draining...")
	}

	stop() // restore default signal handling so a second Ctrl-C force-quits
	hub.closeAll()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Println("http shutdown error:", err)
	}
	if err := uptime.Flush(); err != nil {
		log.Println("uptime flush error:", err)
	}
	log.Println("stopped")
}

func collectLoop(ctx context.Context, cfg Config, hub *Hub, collector *Collector, notifier *Notifier) {
	ticker := time.NewTicker(time.Duration(cfg.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		if collector.uptime != nil {
			if err := collector.uptime.heartbeat(cfg.IntervalSeconds); err != nil {
				log.Println("uptime persist error:", err)
			}
		}
		snap := collector.collect(cfg)
		snapshotStore.Store(&snap)
		data, err := json.Marshal(snap)
		if err != nil {
			log.Println("marshal error:", err)
		} else {
			latest.Store(data)
			ready.Store(true)
			hub.broadcast(data)
			if debugMode() {
				temp := "null"
				if snap.CPU.TemperatureC != nil {
					temp = fmt.Sprintf("%.1fC", *snap.CPU.TemperatureC)
				}
				debugf("snapshot: cpu=%.1f%% temp=%s mem=%.1f%% disks=%d nics=%d clients=%d bytes=%d",
					snap.CPU.UsagePercent, temp, snap.Memory.UsedPercent,
					len(snap.Storage), len(snap.Network), hub.count(), len(data))
			}
		}
		notifier.Check(ctx, snap)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// remoteIP extracts the client IP. it honors the first X-Forwarded-For hop
// only when trustProxy is set; otherwise it uses the transport address, since
// clients can spoof the header to dodge per-IP limits.
func remoteIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, found := strings.Cut(xff, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func latestSnapshot() Snapshot {
	if s, ok := snapshotStore.Load().(*Snapshot); ok && s != nil {
		return *s
	}
	return Snapshot{}
}

// runHealthcheck does a one-shot request to the local /healthz endpoint and
// returns a process exit code (0 = healthy), for the container HEALTHCHECK.
func runHealthcheck(cfg Config) int {
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		port = "8090"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port))

	client := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			// probe hits our own loopback listener; skip cert verification
			// so self-signed TLS setups still pass.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck got status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
