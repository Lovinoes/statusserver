package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("INTERVAL_SECONDS", "10")
	t.Setenv("AUTH_TOKEN", "secret")
	t.Setenv("ALLOWED_ORIGINS", "https://a.example, https://b.example")
	t.Setenv("DISKS", "/,/data")
	t.Setenv("NETWORKS", "eth0")
	t.Setenv("NETWORK_MAX_MBPS", "eth0=1000,eth1=500")
	t.Setenv("MAX_CONNS_PER_IP", "3")
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	t.Setenv("DEBUG", "1")
	t.Setenv("ALERT_WEBHOOK_URL", "https://hook.example/x")
	t.Setenv("ALERT_WEBHOOK_FORMAT", "Discord")
	t.Setenv("ALERT_CPU_PERCENT", "90")
	t.Setenv("ALERT_TEMP_C", "85.5")

	cfg := defaultConfig()
	applyEnv(&cfg)
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.ListenAddr != ":9999" || cfg.IntervalSeconds != 10 || cfg.AuthToken != "secret" {
		t.Errorf("scalars = %q %d %q", cfg.ListenAddr, cfg.IntervalSeconds, cfg.AuthToken)
	}
	if !slices.Equal(cfg.AllowedOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("AllowedOrigins = %v", cfg.AllowedOrigins)
	}
	if !slices.Equal(cfg.Disks, []string{"/", "/data"}) || !slices.Equal(cfg.Networks, []string{"eth0"}) {
		t.Errorf("Disks = %v, Networks = %v", cfg.Disks, cfg.Networks)
	}
	if cfg.NetworkMaxMbps["eth0"] != 1000 || cfg.NetworkMaxMbps["eth1"] != 500 {
		t.Errorf("NetworkMaxMbps = %v", cfg.NetworkMaxMbps)
	}
	if cfg.MaxConnsPerIP != 3 || !cfg.TrustProxyHeaders || !cfg.Debug {
		t.Errorf("MaxConnsPerIP=%d TrustProxyHeaders=%v Debug=%v", cfg.MaxConnsPerIP, cfg.TrustProxyHeaders, cfg.Debug)
	}
	a := cfg.Alerts
	if a.WebhookURL != "https://hook.example/x" || a.WebhookFormat != "discord" || a.CPUPercent != 90 || a.TempC != 85.5 {
		t.Errorf("Alerts = %+v", a)
	}
}

func TestApplyEnvKeepsValueOnParseError(t *testing.T) {
	t.Setenv("INTERVAL_SECONDS", "ten")
	t.Setenv("DEBUG", "maybe")
	t.Setenv("ALERT_CPU_PERCENT", "lots")
	cfg := defaultConfig()
	cfg.Alerts.CPUPercent = 70
	applyEnv(&cfg)
	if cfg.IntervalSeconds != defaultInterval || cfg.Debug || cfg.Alerts.CPUPercent != 70 {
		t.Errorf("bad env values should be ignored, got %d %v %v", cfg.IntervalSeconds, cfg.Debug, cfg.Alerts.CPUPercent)
	}
}

func TestNormalizeClampsInvalid(t *testing.T) {
	cfg := Config{IntervalSeconds: -1, MaxConnsPerIP: -5, Alerts: AlertConfig{CPUPercent: -3, WebhookFormat: "teams"}}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8090" || cfg.IntervalSeconds != 5 || cfg.UptimeFile != "uptime.json" || cfg.MaxConnsPerIP != 0 {
		t.Errorf("got %+v", cfg)
	}
	if !slices.Equal(cfg.AllowedOrigins, []string{"*"}) {
		t.Errorf("AllowedOrigins = %v", cfg.AllowedOrigins)
	}
	if cfg.Alerts.CPUPercent != 0 || cfg.Alerts.WebhookFormat != "generic" {
		t.Errorf("Alerts = %+v", cfg.Alerts)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := map[string]Config{
		"cert without key":   {TLSCert: "c"},
		"key without cert":   {TLSKey: "k"},
		"bad origin pattern": {AllowedOrigins: []string{"https://[bad"}},
		"relative webhook":   {Alerts: AlertConfig{WebhookURL: "hooks.example/abc"}},
		"non-http webhook":   {Alerts: AlertConfig{WebhookURL: "ftp://hooks.example/abc"}},
	}
	for name, cfg := range cases {
		if err := normalizeConfig(&cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNormalizeWebhookErrorRedactsSecret(t *testing.T) {
	cfg := Config{Alerts: AlertConfig{WebhookURL: "ftp://hooks.example/SECRET"}}
	err := normalizeConfig(&cfg)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error should exist and not leak the url: %v", err)
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
	m := parseMbpsMap("eth0=1000, eth1 = 2.5 ,bad,=5,eth2=x,eth3=-1")
	if m["eth0"] != 1000 || m["eth1"] != 2.5 || len(m) != 2 {
		t.Errorf("got %v, want eth0=1000 eth1=2.5 only", m)
	}
	if parseMbpsMap("") != nil {
		t.Error("empty input should give nil")
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" a, ,b ,"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("splitList = %v", got)
	}
	if splitList(" , ") != nil {
		t.Error("blank list should be nil")
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigFileThenEnv(t *testing.T) {
	p := writeFile(t, "config.json", "\xef\xbb\xbf"+`{
		"listen_addr": "127.0.0.1:7000",
		"interval_seconds": 2,
		"auth_token": "from-file",
		"typo_key": true,
		"alerts": {"webhook_url": "https://hook.example/a", "cpu_percent": 80}
	}`)
	t.Setenv("AUTH_TOKEN", "from-env")

	cfg, err := loadConfig(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:7000" || cfg.IntervalSeconds != 2 {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if cfg.AuthToken != "from-env" {
		t.Errorf("env should win, AuthToken = %q", cfg.AuthToken)
	}
	if cfg.UptimeFile != "uptime.json" || !slices.Equal(cfg.AllowedOrigins, []string{"*"}) {
		t.Errorf("defaults for unset keys lost: %+v", cfg)
	}
	if cfg.Alerts.CPUPercent != 80 {
		t.Errorf("nested alerts not applied: %+v", cfg.Alerts)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := loadConfig(missing, false); err != nil {
		t.Errorf("implicit missing config should be fine, got %v", err)
	}
	if _, err := loadConfig(missing, true); err == nil {
		t.Error("explicitly requested missing config should fail")
	}
	if _, err := loadConfig(writeFile(t, "bad.json", `{"listen_addr": `), false); err == nil {
		t.Error("syntax error should fail")
	}
	if _, err := loadConfig(writeFile(t, "type.json", `{"interval_seconds": "5"}`), false); err == nil {
		t.Error("wrong type should fail")
	}
}

func TestRedactedString(t *testing.T) {
	cfg := Config{AuthToken: "supersecret", Alerts: AlertConfig{WebhookURL: "https://discord.com/api/webhooks/1/TOKEN"}}
	s := cfg.redactedString()
	if strings.Contains(s, "supersecret") || strings.Contains(s, "TOKEN") {
		t.Errorf("redactedString leaked a secret: %s", s)
	}
	if cfg.AuthToken != "supersecret" {
		t.Error("redactedString must not modify the caller's config")
	}
}
