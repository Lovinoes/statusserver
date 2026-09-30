package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	defaultListenAddr = ":8090"
	defaultInterval   = 5
	defaultUptimeFile = "uptime.json"
)

// Config is the resolved runtime configuration: defaults, then optional json
// file, then environment variables (env wins). each env var maps to a json
// key, upper-cased.
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

	// set both to serve https/wss directly; empty serves plain http.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`

	MaxConnsPerIP int `json:"max_conns_per_ip"` // per-IP websocket cap (0 = unlimited)

	// trust X-Forwarded-For for client ip. enable only behind a trusted
	// reverse proxy; otherwise clients can spoof it and dodge MaxConnsPerIP.
	TrustProxyHeaders bool `json:"trust_proxy_headers"`

	// debug turns on verbose logging (request headers, collection detail, etc).
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
		ListenAddr:      defaultListenAddr,
		IntervalSeconds: defaultInterval,
		AllowedOrigins:  []string{"*"},
		UptimeFile:      defaultUptimeFile,
	}
}

// loadConfig resolves config from the given file overlaid with environment
// variables. A missing file is fine unless the path was given explicitly.
func loadConfig(path string, explicit bool) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := parseConfigFile(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		log.Printf("no config file at %s, using defaults + environment", path)
	default:
		return cfg, fmt.Errorf("read config: %w", err)
	}

	applyEnv(&cfg)
	if err := normalizeConfig(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseConfigFile decodes a json config over cfg. Syntax errors are fatal;
// unknown keys (usually typos) are only warned about so a config written for
// a newer version still loads.
func parseConfigFile(data []byte, cfg *Config) error {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // BOM from windows editors
	if err := json.Unmarshal(data, cfg); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var probe Config
	if err := dec.Decode(&probe); err != nil {
		log.Printf("WARNING: config: %v (ignored)", err)
	}
	return nil
}

// applyEnv overlays any set environment variables onto cfg.
func applyEnv(cfg *Config) {
	envString("LISTEN_ADDR", &cfg.ListenAddr)
	envInt("INTERVAL_SECONDS", &cfg.IntervalSeconds)
	envString("AUTH_TOKEN", &cfg.AuthToken)
	envList("ALLOWED_ORIGINS", &cfg.AllowedOrigins)
	envList("DISKS", &cfg.Disks)
	envList("NETWORKS", &cfg.Networks)
	if v, ok := os.LookupEnv("NETWORK_MAX_MBPS"); ok {
		cfg.NetworkMaxMbps = parseMbpsMap(v)
	}
	envString("TEMP_SENSOR_MATCH", &cfg.TempSensorMatch)
	envString("UPTIME_FILE", &cfg.UptimeFile)
	envString("TLS_CERT", &cfg.TLSCert)
	envString("TLS_KEY", &cfg.TLSKey)
	envInt("MAX_CONNS_PER_IP", &cfg.MaxConnsPerIP)
	envBool("TRUST_PROXY_HEADERS", &cfg.TrustProxyHeaders)
	envBool("DEBUG", &cfg.Debug)

	envString("ALERT_WEBHOOK_URL", &cfg.Alerts.WebhookURL)
	envString("ALERT_WEBHOOK_FORMAT", &cfg.Alerts.WebhookFormat)
	envFloat("ALERT_CPU_PERCENT", &cfg.Alerts.CPUPercent)
	envFloat("ALERT_MEMORY_PERCENT", &cfg.Alerts.MemoryPercent)
	envFloat("ALERT_DISK_PERCENT", &cfg.Alerts.DiskPercent)
	envFloat("ALERT_TEMP_C", &cfg.Alerts.TempC)
}

// normalizeConfig clamps out-of-range values back to safe defaults (with a
// warning) and returns an error for settings that can't work at all.
func normalizeConfig(cfg *Config) error {
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.IntervalSeconds <= 0 {
		log.Printf("WARNING: interval_seconds %d is invalid, using %d", cfg.IntervalSeconds, defaultInterval)
		cfg.IntervalSeconds = defaultInterval
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{"*"}
	}
	for _, o := range cfg.AllowedOrigins {
		if _, err := path.Match(o, ""); err != nil {
			return fmt.Errorf("allowed_origins: bad pattern %q: %w", o, err)
		}
	}
	if cfg.UptimeFile == "" {
		cfg.UptimeFile = defaultUptimeFile
	}
	if cfg.MaxConnsPerIP < 0 {
		log.Printf("WARNING: max_conns_per_ip %d is negative, using 0 (unlimited)", cfg.MaxConnsPerIP)
		cfg.MaxConnsPerIP = 0
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return errors.New("tls_cert and tls_key must both be set, or both be empty")
	}

	a := &cfg.Alerts
	a.WebhookFormat = strings.ToLower(strings.TrimSpace(a.WebhookFormat))
	switch a.WebhookFormat {
	case "", "generic", "discord", "slack":
	default:
		log.Printf("WARNING: unknown alerts.webhook_format %q, using generic", a.WebhookFormat)
		a.WebhookFormat = "generic"
	}
	for _, t := range []*float64{&a.CPUPercent, &a.MemoryPercent, &a.DiskPercent, &a.TempC} {
		if *t < 0 {
			log.Printf("WARNING: negative alert threshold %v, disabling that check", *t)
			*t = 0
		}
	}
	if a.WebhookURL != "" {
		u, err := url.Parse(a.WebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("alerts.webhook_url must be an absolute http(s) URL (got %s)", redactURL(a.WebhookURL))
		}
		if !a.anyThreshold() {
			log.Println("WARNING: alerts.webhook_url is set but every threshold is 0; alerts stay off")
		}
	}
	return nil
}

func (a AlertConfig) anyThreshold() bool {
	return a.CPUPercent > 0 || a.MemoryPercent > 0 || a.DiskPercent > 0 || a.TempC > 0
}

// TLSEnabled reports whether HTTPS should be served.
func (c Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }

// redactedString renders the config for debug logging with secrets masked.
// Value receiver, so we mask our own copy.
func (c Config) redactedString() string {
	if c.AuthToken != "" {
		c.AuthToken = "(set, len " + strconv.Itoa(len(c.AuthToken)) + ")"
	}
	if c.Alerts.WebhookURL != "" {
		c.Alerts.WebhookURL = redactURL(c.Alerts.WebhookURL)
	}
	return fmt.Sprintf("%+v", c)
}

// splitList parses a comma-separated value into a trimmed, non-empty slice;
// empty input yields nil ("unset / auto").
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseMbpsMap parses "eth0=1000,eth1=500" into a map, skipping (and warning
// about) malformed pairs.
func parseMbpsMap(v string) map[string]float64 {
	out := map[string]float64{}
	for _, pair := range splitList(v) {
		k, val, found := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if !found || k == "" || err != nil || f <= 0 {
			log.Printf("WARNING: NETWORK_MAX_MBPS: ignoring malformed entry %q", pair)
			continue
		}
		out[k] = f
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envString(name string, dst *string) {
	if v, ok := os.LookupEnv(name); ok {
		*dst = v
	}
}

func envList(name string, dst *[]string) {
	if v, ok := os.LookupEnv(name); ok {
		*dst = splitList(v)
	}
}

func envInt(name string, dst *int) {
	if v, ok := os.LookupEnv(name); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			log.Printf("WARNING: %s=%q is not an integer, ignoring", name, v)
			return
		}
		*dst = n
	}
}

func envFloat(name string, dst *float64) {
	if v, ok := os.LookupEnv(name); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			log.Printf("WARNING: %s=%q is not a number, ignoring", name, v)
			return
		}
		*dst = f
	}
}

func envBool(name string, dst *bool) {
	if v, ok := os.LookupEnv(name); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			log.Printf("WARNING: %s=%q is not a boolean, ignoring", name, v)
			return
		}
		*dst = b
	}
}
