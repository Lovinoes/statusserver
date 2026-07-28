package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Config is the resolved runtime configuration, built in three layers:
// defaults, optional JSON file, then environment variables (env wins).
// Each env var maps 1:1 to a JSON key, upper-cased.
type Config struct {
	ListenAddr      string             `json:"listen_addr"`
	IntervalSeconds int                `json:"interval_seconds"`
	AuthToken       string             `json:"auth_token"`
	AllowedOrigins  []string           `json:"allowed_origins"`
	Disks           []string           `json:"disks"`             // mountpoints; empty = all local
	Networks        []string           `json:"networks"`          // interfaces; empty = all non-virtual
	NetworkMaxMbps  map[string]float64 `json:"network_max_mbps"`  // optional per-NIC link capacity
	TempSensorMatch string             `json:"temp_sensor_match"` // sensor key substring, e.g. "coretemp"
	UptimeFile      string             `json:"uptime_file"`       // empty = "uptime.json"

	// Set both to serve HTTPS/WSS directly; empty serves plain HTTP.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`

	MaxConnsPerIP int `json:"max_conns_per_ip"` // per-IP websocket cap (0 = unlimited)

	// Trust X-Forwarded-For for client IP. Enable only behind a trusted
	// reverse proxy; otherwise clients can spoof it and evade MaxConnsPerIP.
	TrustProxyHeaders bool `json:"trust_proxy_headers"`

	// Debug turns on verbose logging: request headers, per-tick collection
	// detail, alert evaluation, websocket lifecycle, etc.
	Debug bool `json:"debug"`

	Alerts AlertConfig `json:"alerts"`
}

// AlertConfig sets thresholds and webhook target. A threshold of 0 disables it.
type AlertConfig struct {
	WebhookURL    string  `json:"webhook_url"`
	WebhookFormat string  `json:"webhook_format"` // "discord", "slack", or "generic"
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	DiskPercent   float64 `json:"disk_percent"`
	TempC         float64 `json:"temp_c"`
}

func defaultConfig() Config {
	return Config{
		ListenAddr:      ":8090",
		IntervalSeconds: 5,
		AllowedOrigins:  []string{"*"},
		UptimeFile:      "uptime.json",
	}
}

// loadConfig resolves config from the given file path (may not exist)
// overlaid with environment variables.
func loadConfig(path string) Config {
	cfg := defaultConfig()

	if f, err := os.Open(path); err != nil {
		log.Printf("no config file at %s, using defaults + environment", path)
	} else {
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&cfg); err != nil {
			log.Fatalf("failed to parse config %s: %v", path, err)
		}
	}

	applyEnv(&cfg)
	normalizeConfig(&cfg)
	return cfg
}

// applyEnv overlays set environment variables onto cfg. Only variables that
// are present take effect, so env overrides the file selectively.
func applyEnv(cfg *Config) {
	if v, ok := os.LookupEnv("LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}
	if v, ok := os.LookupEnv("INTERVAL_SECONDS"); ok {
		cfg.IntervalSeconds = atoiOr(v, cfg.IntervalSeconds)
	}
	if v, ok := os.LookupEnv("AUTH_TOKEN"); ok {
		cfg.AuthToken = v
	}
	if v, ok := os.LookupEnv("ALLOWED_ORIGINS"); ok {
		cfg.AllowedOrigins = splitList(v)
	}
	if v, ok := os.LookupEnv("DISKS"); ok {
		cfg.Disks = splitList(v)
	}
	if v, ok := os.LookupEnv("NETWORKS"); ok {
		cfg.Networks = splitList(v)
	}
	if v, ok := os.LookupEnv("NETWORK_MAX_MBPS"); ok {
		cfg.NetworkMaxMbps = parseMbpsMap(v)
	}
	if v, ok := os.LookupEnv("TEMP_SENSOR_MATCH"); ok {
		cfg.TempSensorMatch = v
	}
	if v, ok := os.LookupEnv("UPTIME_FILE"); ok {
		cfg.UptimeFile = v
	}
	if v, ok := os.LookupEnv("TLS_CERT"); ok {
		cfg.TLSCert = v
	}
	if v, ok := os.LookupEnv("TLS_KEY"); ok {
		cfg.TLSKey = v
	}
	if v, ok := os.LookupEnv("MAX_CONNS_PER_IP"); ok {
		cfg.MaxConnsPerIP = atoiOr(v, cfg.MaxConnsPerIP)
	}
	if v, ok := os.LookupEnv("TRUST_PROXY_HEADERS"); ok {
		cfg.TrustProxyHeaders = atobOr(v, cfg.TrustProxyHeaders)
	}
	if v, ok := os.LookupEnv("DEBUG"); ok {
		cfg.Debug = atobOr(v, cfg.Debug)
	}

	if v, ok := os.LookupEnv("ALERT_WEBHOOK_URL"); ok {
		cfg.Alerts.WebhookURL = v
	}
	if v, ok := os.LookupEnv("ALERT_WEBHOOK_FORMAT"); ok {
		cfg.Alerts.WebhookFormat = v
	}
	if v, ok := os.LookupEnv("ALERT_CPU_PERCENT"); ok {
		cfg.Alerts.CPUPercent = atofOr(v, cfg.Alerts.CPUPercent)
	}
	if v, ok := os.LookupEnv("ALERT_MEMORY_PERCENT"); ok {
		cfg.Alerts.MemoryPercent = atofOr(v, cfg.Alerts.MemoryPercent)
	}
	if v, ok := os.LookupEnv("ALERT_DISK_PERCENT"); ok {
		cfg.Alerts.DiskPercent = atofOr(v, cfg.Alerts.DiskPercent)
	}
	if v, ok := os.LookupEnv("ALERT_TEMP_C"); ok {
		cfg.Alerts.TempC = atofOr(v, cfg.Alerts.TempC)
	}
}

// normalizeConfig clamps invalid values back to safe defaults.
func normalizeConfig(cfg *Config) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8090"
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 5
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{"*"}
	}
	if cfg.UptimeFile == "" {
		cfg.UptimeFile = "uptime.json"
	}
	if cfg.MaxConnsPerIP < 0 {
		cfg.MaxConnsPerIP = 0
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		log.Fatal("tls_cert and tls_key must both be set, or both be empty")
	}
}

// TLSEnabled reports whether HTTPS should be served.
func (c Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }

// redactedString renders the config for debug logging with secrets masked, so
// the full effective configuration can be inspected without leaking the auth
// token or webhook URL.
func (c Config) redactedString() string {
	authToken := "(empty)"
	if c.AuthToken != "" {
		authToken = "(set, len " + strconv.Itoa(len(c.AuthToken)) + ")"
	}
	webhook := "(empty)"
	if c.Alerts.WebhookURL != "" {
		webhook = redactURL(c.Alerts.WebhookURL)
	}
	return fmt.Sprintf(
		"listen_addr=%q interval_seconds=%d auth_token=%s allowed_origins=%v "+
			"disks=%v networks=%v network_max_mbps=%v temp_sensor_match=%q "+
			"uptime_file=%q tls=%t max_conns_per_ip=%d trust_proxy_headers=%t debug=%t "+
			"alerts{webhook=%s format=%q cpu=%.1f memory=%.1f disk=%.1f temp=%.1f}",
		c.ListenAddr, c.IntervalSeconds, authToken, c.AllowedOrigins,
		c.Disks, c.Networks, c.NetworkMaxMbps, c.TempSensorMatch,
		c.UptimeFile, c.TLSEnabled(), c.MaxConnsPerIP, c.TrustProxyHeaders, c.Debug,
		webhook, c.Alerts.WebhookFormat, c.Alerts.CPUPercent, c.Alerts.MemoryPercent,
		c.Alerts.DiskPercent, c.Alerts.TempC,
	)
}

// splitList parses a comma-separated value into a trimmed, non-empty slice;
// empty input yields nil ("unset / auto").
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseMbpsMap parses "eth0=1000,eth1=500" into a map.
func parseMbpsMap(v string) map[string]float64 {
	out := map[string]float64{}
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, val, found := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !found || k == "" {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return fallback
}

func atofOr(s string, fallback float64) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return f
	}
	return fallback
}

func atobOr(s string, fallback bool) bool {
	if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
		return b
	}
	return fallback
}
