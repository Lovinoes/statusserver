package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("INTERVAL_SECONDS", "10")
	t.Setenv("AUTH_TOKEN", "secret")
	t.Setenv("ALLOWED_ORIGINS", "https://a.example, https://b.example")
	t.Setenv("DISKS", "/,/data")
	t.Setenv("NETWORK_MAX_MBPS", "eth0=1000,eth1=500")
	t.Setenv("MAX_CONNS_PER_IP", "3")
	t.Setenv("ALERT_WEBHOOK_URL", "https://hook.example")
	t.Setenv("ALERT_CPU_PERCENT", "90")

	cfg := defaultConfig()
	applyEnv(&cfg)
	normalizeConfig(&cfg)

	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999", cfg.ListenAddr)
	}
	if cfg.IntervalSeconds != 10 {
		t.Errorf("IntervalSeconds = %d, want 10", cfg.IntervalSeconds)
	}
	if cfg.AuthToken != "secret" {
		t.Errorf("AuthToken = %q, want secret", cfg.AuthToken)
	}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != "https://a.example" {
		t.Errorf("AllowedOrigins = %v", cfg.AllowedOrigins)
	}
	if len(cfg.Disks) != 2 || cfg.Disks[1] != "/data" {
		t.Errorf("Disks = %v", cfg.Disks)
	}
	if cfg.NetworkMaxMbps["eth0"] != 1000 || cfg.NetworkMaxMbps["eth1"] != 500 {
		t.Errorf("NetworkMaxMbps = %v", cfg.NetworkMaxMbps)
	}
	if cfg.MaxConnsPerIP != 3 {
		t.Errorf("MaxConnsPerIP = %d, want 3", cfg.MaxConnsPerIP)
	}
	if cfg.Alerts.WebhookURL != "https://hook.example" || cfg.Alerts.CPUPercent != 90 {
		t.Errorf("Alerts = %+v", cfg.Alerts)
	}
}

func TestNormalizeClampsInvalid(t *testing.T) {
	cfg := Config{IntervalSeconds: -1, MaxConnsPerIP: -5}
	normalizeConfig(&cfg)
	if cfg.ListenAddr != ":8090" {
		t.Errorf("ListenAddr = %q, want :8090", cfg.ListenAddr)
	}
	if cfg.IntervalSeconds != 5 {
		t.Errorf("IntervalSeconds = %d, want 5", cfg.IntervalSeconds)
	}
	if cfg.UptimeFile != "uptime.json" {
		t.Errorf("UptimeFile = %q, want uptime.json", cfg.UptimeFile)
	}
	if len(cfg.AllowedOrigins) != 1 || cfg.AllowedOrigins[0] != "*" {
		t.Errorf("AllowedOrigins = %v", cfg.AllowedOrigins)
	}
	if cfg.MaxConnsPerIP != 0 {
		t.Errorf("MaxConnsPerIP = %d, want 0", cfg.MaxConnsPerIP)
	}
}

func TestTLSEnabled(t *testing.T) {
	if (Config{}).TLSEnabled() {
		t.Error("empty config should not have TLS enabled")
	}
	if !(Config{TLSCert: "c", TLSKey: "k"}).TLSEnabled() {
		t.Error("cert+key should enable TLS")
	}
}

func TestParseMbpsMap(t *testing.T) {
	m := parseMbpsMap("eth0=1000, eth1 = 2.5 ,bad,=5,eth2=x")
	if m["eth0"] != 1000 {
		t.Errorf("eth0 = %v, want 1000", m["eth0"])
	}
	if m["eth1"] != 2.5 {
		t.Errorf("eth1 = %v, want 2.5", m["eth1"])
	}
	if _, ok := m["eth2"]; ok {
		t.Error("eth2 with non-numeric value should be dropped")
	}
	if len(m) != 2 {
		t.Errorf("map size = %d, want 2", len(m))
	}
}

func TestRemoteIP(t *testing.T) {
	cases := []struct {
		xff        string
		remote     string
		trustProxy bool
		want       string
	}{
		{"", "1.2.3.4:5555", false, "1.2.3.4"},
		{"", "1.2.3.4:5555", true, "1.2.3.4"},
		// Trusted proxy: honor the first XFF hop.
		{"9.9.9.9, 10.0.0.1", "1.2.3.4:5555", true, "9.9.9.9"},
		{"8.8.8.8", "1.2.3.4:5555", true, "8.8.8.8"},
		// Untrusted: ignore spoofable XFF, use transport address.
		{"9.9.9.9, 10.0.0.1", "1.2.3.4:5555", false, "1.2.3.4"},
		{"8.8.8.8", "1.2.3.4:5555", false, "1.2.3.4"},
	}
	for _, c := range cases {
		r := &http.Request{Header: http.Header{}, RemoteAddr: c.remote}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := remoteIP(r, c.trustProxy); got != c.want {
			t.Errorf("remoteIP(xff=%q, remote=%q, trust=%v) = %q, want %q", c.xff, c.remote, c.trustProxy, got, c.want)
		}
	}
}

func TestWriteMetricsContents(t *testing.T) {
	temp := 55.5
	snap := Snapshot{
		Timestamp: time.Unix(1700000000, 0),
		CPU:       CPUInfo{UsagePercent: 42.5, TemperatureC: &temp},
		Memory:    MemoryInfo{TotalBytes: 1000, UsedBytes: 500, UsedPercent: 50},
		Storage:   []StorageInfo{{Mount: "/", Device: "sda1", Fstype: "ext4", UsedPercent: 73.2, TotalBytes: 100, UsedBytes: 73}},
		Network:   []NetworkInfo{{Interface: "eth0", RxBytesPerSec: 12.5, TxBytesPerSec: 8, RxTotalBytes: 100, TxTotalBytes: 200}},
		Uptime:    UptimeInfo{Seconds: 3600, Percent7d: 99.9},
	}
	var sb strings.Builder
	writeMetrics(&sb, snap, 2)
	out := sb.String()

	wants := []string{
		"build_info{version=",
		"ws_clients 2",
		"cpu_usage_percent 42.5",
		"cpu_temperature_celsius 55.5",
		"memory_used_percent 50",
		`disk_used_percent{mount="/",device="sda1",fstype="ext4"} 73.2`,
		`network_rx_bytes_per_second{interface="eth0"} 12.5`,
		"host_uptime_seconds 3600",
		`agent_uptime_percent{window="7d"} 99.9`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("metrics output missing %q\n---\n%s", w, out)
		}
	}
}

func TestWriteMetricsEmptySnapshot(t *testing.T) {
	var sb strings.Builder
	writeMetrics(&sb, Snapshot{}, 0)
	out := sb.String()
	if !strings.Contains(out, "build_info") {
		t.Error("build_info should always be present")
	}
	if strings.Contains(out, "cpu_usage_percent") {
		t.Error("host metrics should be omitted for a zero snapshot")
	}
}

func TestNotifierDisabledIsNoop(t *testing.T) {
	n := NewNotifier(AlertConfig{}) // no webhook
	if n.enabled {
		t.Error("notifier without webhook should be disabled")
	}
	// Should not panic or block.
	n.Check(context.Background(), Snapshot{CPU: CPUInfo{UsagePercent: 100}})
}

func TestNotifierEdgeTrigger(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "https://hook.example", CPUPercent: 80})
	if !n.enabled {
		t.Fatal("notifier should be enabled")
	}

	// Below threshold: no state.
	n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 50}})
	if n.isFiring("cpu") {
		t.Error("cpu should not be firing at 50%")
	}
	// Cross threshold: firing.
	n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 90}})
	if !n.isFiring("cpu") {
		t.Error("cpu should be firing at 90%")
	}
	// Recover.
	n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 10}})
	if n.isFiring("cpu") {
		t.Error("cpu should have recovered at 10%")
	}
}

// isFiring is a test helper exposing the notifier's internal breach state.
func (n *Notifier) isFiring(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.firing[key]
}

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://discord.com/api/webhooks/123/abcSECRETtoken", "https://discord.com/[redacted]"},
		{"https://hooks.slack.com/services/T00/B00/XXXSECRET", "https://hooks.slack.com/[redacted]"},
		{"https://user:pass@example.com/path?token=secret", "https://example.com/[redacted]"},
		{"", "[redacted]"},
		{"not a url", "[redacted]"},
	}
	for _, c := range cases {
		got := redactURL(c.in)
		if got != c.want {
			t.Errorf("redactURL(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, "SECRET") || strings.Contains(got, "secret") || strings.Contains(got, "pass") {
			t.Errorf("redactURL(%q) leaked a secret: %q", c.in, got)
		}
	}
}
