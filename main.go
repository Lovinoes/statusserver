package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Config is loaded from a JSON file. Every field has a sane default so the
// server runs with zero configuration, but you'll usually want to set at
// least auth_token and allowed_origins before exposing it publicly.
type Config struct {
	ListenAddr      string             `json:"listen_addr"`
	IntervalSeconds int                `json:"interval_seconds"`
	AuthToken       string             `json:"auth_token"`
	AllowedOrigins  []string           `json:"allowed_origins"`
	Disks           []string           `json:"disks"`             // mountpoints to watch; empty = all local partitions
	Networks        []string           `json:"networks"`          // interface names to watch; empty = all non-virtual
	NetworkMaxMbps  map[string]float64 `json:"network_max_mbps"`  // optional, for showing % of link capacity
	TempSensorMatch string             `json:"temp_sensor_match"` // substring match against sensor key, e.g. "coretemp"
}

func loadConfig(path string) Config {
	cfg := Config{
		ListenAddr:      ":8090",
		IntervalSeconds: 5,
		AllowedOrigins:  []string{"*"},
	}
	f, err := os.Open(path)
	if err != nil {
		log.Printf("no config file at %s, using defaults", path)
		return cfg
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		log.Fatalf("failed to parse config %s: %v", path, err)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8090"
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 5
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{"*"}
	}
	return cfg
}

// Hub tracks connected websocket clients and broadcasts snapshots to them.
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
}

func newHub() *Hub { return &Hub{clients: make(map[*websocket.Conn]struct{})} }

func (h *Hub) add(c *websocket.Conn) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) broadcast(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			go c.CloseNow()
			delete(h.clients, c)
		}
	}
}

// latest holds the most recent snapshot as already-marshalled JSON so both
// the polling endpoint and new websocket clients can serve it instantly.
var latest atomic.Value

func checkAuth(cfg Config, r *http.Request) bool {
	if cfg.AuthToken == "" {
		return true
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return tok == cfg.AuthToken
}

func corsAllowed(cfg Config, origin string) bool {
	for _, o := range cfg.AllowedOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg := loadConfig(*configPath)
	hub := newHub()
	latest.Store([]byte(`{}`))

	go collectLoop(cfg, hub)

	mux := http.NewServeMux()

	// Plain HTTP pull endpoint - returns the latest snapshot.
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && corsAllowed(cfg, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		if !checkAuth(cfg, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(latest.Load().([]byte))
	})

	// Websocket push endpoint - sends a snapshot immediately, then every
	// interval afterwards.
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
		hub.add(c)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.Write(ctx, websocket.MessageText, latest.Load().([]byte))
		cancel()

		// We never expect data from the client. CloseRead spins up a
		// background reader that handles control frames and cancels this
		// context once the connection closes - exactly what a write-only
		// server needs.
		closeCtx := c.CloseRead(context.Background())
		<-closeCtx.Done()
		hub.remove(c)
	})

	log.Printf("status server listening on %s (interval %ds)", cfg.ListenAddr, cfg.IntervalSeconds)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func collectLoop(cfg Config, hub *Hub) {
	ticker := time.NewTicker(time.Duration(cfg.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		snap := collect(cfg)
		data, err := json.Marshal(snap)
		if err != nil {
			log.Println("marshal error:", err)
		} else {
			latest.Store(data)
			hub.broadcast(data)
		}
		<-ticker.C
	}
}
