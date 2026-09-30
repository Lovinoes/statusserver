package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
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

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.json", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit (for container HEALTHCHECK)")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return 0
	}

	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})

	if *healthcheck {
		// keep the probe's output (stored by docker) to the verdict only.
		log.SetOutput(io.Discard)
		cfg, err := loadConfig(*configPath, explicit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			return 1
		}
		return runHealthcheck(cfg)
	}

	log.Printf("starting %s", versionString())
	cfg, err := loadConfig(*configPath, explicit)
	if err != nil {
		log.Printf("FATAL: %v", err)
		return 1
	}
	setDebug(cfg.Debug)
	if cfg.Debug {
		log.Println("debug logging enabled")
		debugf("effective config: %s", cfg.redactedString())
	}
	warnInsecureExposure(cfg)

	if err := serve(cfg); err != nil {
		log.Printf("FATAL: %v", err)
		return 1
	}
	return 0
}

// serve runs the agent until SIGINT/SIGTERM or a fatal server error.
func serve(cfg Config) error {
	srv := newServer(cfg)
	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// no overall WriteTimeout: it would kill long-lived /ws connections.
	}

	// load TLS material and bind the port before anything else starts, so
	// a bad cert path or a port in use fails immediately.
	scheme := "http"
	if cfg.TLSEnabled() {
		certs, err := newCertReloader(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return err
		}
		httpSrv.TLSConfig = hardenedTLSConfig(certs.GetCertificate)
		scheme = "https"
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	uptime := loadUptimeStore(cfg.UptimeFile)
	collector := NewCollector(cfg, uptime)
	notifier := NewNotifier(cfg.Alerts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// background work gets its own context so it can be stopped only after
	// the http server has drained.
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	go notifier.Run(workCtx)
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		srv.collectLoop(workCtx, collector, uptime, notifier)
	}()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("status server listening on %s (%s, interval %ds)", ln.Addr(), scheme, cfg.IntervalSeconds)
		if cfg.TLSEnabled() {
			log.Printf("TLS profile: TLS 1.3 only, key exchange X25519MLKEM768/X25519/secp384r1, ALPN h2,http/1.1")
			serverErr <- httpSrv.ServeTLS(ln, "", "")
		} else {
			serverErr <- httpSrv.Serve(ln)
		}
	}()

	var runErr error
	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
		log.Println("shutdown signal received, draining...")
	}
	stop() // restore default signal handling so a second Ctrl-C force-quits

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.beginShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Println("http shutdown error:", err)
	}
	// websocket connections are hijacked, so Shutdown doesn't wait for them.
	if err := srv.hub.close(shutdownCtx); err != nil {
		log.Println("websocket shutdown error:", err)
	}

	stopWork()
	select {
	case <-collectDone:
	case <-shutdownCtx.Done():
		log.Println("collector did not stop in time")
	}
	uptime.Heartbeat(time.Now(), uptimeMaxGap(cfg))
	if err := uptime.Flush(); err != nil {
		log.Println("uptime flush error:", err)
	}
	log.Println("stopped")
	return runErr
}

// uptimeMaxGap is the most time one heartbeat may credit. Anything longer
// (process suspended, host asleep) means we weren't reporting.
func uptimeMaxGap(cfg Config) time.Duration {
	return time.Duration(cfg.IntervalSeconds)*time.Second + 30*time.Second
}

// collectLoop samples the system every interval and publishes the result.
func (s *Server) collectLoop(ctx context.Context, col *Collector, uptime *UptimeStore, notifier *Notifier) {
	ticker := time.NewTicker(time.Duration(s.cfg.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	maxGap := uptimeMaxGap(s.cfg)
	for {
		now := time.Now()
		uptime.Heartbeat(now, maxGap)
		if err := uptime.SaveIfDue(now); err != nil {
			log.Println("uptime persist error:", err)
		}

		snap := col.collect(ctx)
		if data, err := json.Marshal(snap); err != nil {
			log.Println("marshal error:", err)
		} else {
			s.publish(&snap, data)
			if debugMode() {
				temp := "null"
				if snap.CPU.TemperatureC != nil {
					temp = fmt.Sprintf("%.1fC", *snap.CPU.TemperatureC)
				}
				debugf("snapshot: cpu=%.1f%% temp=%s mem=%.1f%% disks=%d nics=%d clients=%d bytes=%d took=%s",
					snap.CPU.UsagePercent, temp, snap.Memory.UsedPercent,
					len(snap.Storage), len(snap.Network), s.hub.count(), len(data), time.Since(now).Round(time.Millisecond))
			}
		}
		notifier.Check(snap)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	log.Printf("WARNING: auth_token is empty and listen_addr %q is not loopback; /api/status, /ws and /metrics are exposed without authentication", cfg.ListenAddr)
}

// runHealthcheck does a one-shot request to the local /healthz endpoint and
// returns a process exit code (0 = healthy), for the container HEALTHCHECK.
func runHealthcheck(cfg Config) int {
	url, err := healthcheckURL(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
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

// healthcheckURL builds the /healthz URL for the configured listener,
// swapping a wildcard bind address for loopback.
func healthcheckURL(cfg Config) (string, error) {
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return "", fmt.Errorf("bad listen_addr %q: %w", cfg.ListenAddr, err)
	}
	// go listens dual-stack on "::", so ipv4 loopback reaches it too.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port)), nil
}
