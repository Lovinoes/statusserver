package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNotifierDisabledIsNoop(t *testing.T) {
	for _, cfg := range []AlertConfig{
		{},                            // nothing
		{CPUPercent: 80},              // no webhook
		{WebhookURL: "https://hook/"}, // no thresholds
	} {
		n := NewNotifier(cfg)
		if n.enabled {
			t.Errorf("%+v should be disabled", cfg)
		}
		n.Check(Snapshot{CPU: CPUInfo{UsagePercent: 100}})
		if len(n.queue) != 0 {
			t.Error("disabled notifier queued a message")
		}
	}
}

func TestNotifierEdgeTrigger(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "https://hook.example", CPUPercent: 80, DiskPercent: 90})

	if msgs := n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 50}}); len(msgs) != 0 {
		t.Errorf("below threshold: %v", msgs)
	}
	msgs := n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 90}})
	if len(msgs) != 1 || !strings.Contains(msgs[0], "ALERT: CPU usage is 90.0%") {
		t.Errorf("crossing: %v", msgs)
	}
	if msgs := n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 95}}); len(msgs) != 0 {
		t.Errorf("still breaching must not repeat: %v", msgs)
	}
	msgs = n.evaluate(Snapshot{CPU: CPUInfo{UsagePercent: 10}})
	if len(msgs) != 1 || !strings.Contains(msgs[0], "RECOVERED: CPU usage back to 10.0%") {
		t.Errorf("recovery: %v", msgs)
	}

	disks := Snapshot{Storage: []StorageInfo{{Mount: "/", UsedPercent: 95}, {Mount: "/data", UsedPercent: 20}}}
	msgs = n.evaluate(disks)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Disk usage (/)") {
		t.Errorf("disk: %v", msgs)
	}
}

func TestNotifierTempSkippedWithoutSensor(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "https://hook.example", TempC: 80})
	if msgs := n.evaluate(Snapshot{}); len(msgs) != 0 {
		t.Errorf("nil temperature should not alert: %v", msgs)
	}
	hot := 91.0
	if msgs := n.evaluate(Snapshot{CPU: CPUInfo{TemperatureC: &hot}}); len(msgs) != 1 {
		t.Errorf("hot cpu should alert: %v", msgs)
	}
}

type hookRecorder struct {
	mu       sync.Mutex
	bodies   []map[string]any
	statuses []int // responses to hand out in order; then 204
}

func (h *hookRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	h.bodies = append(h.bodies, m)
	status := http.StatusNoContent
	if len(h.statuses) > 0 {
		status, h.statuses = h.statuses[0], h.statuses[1:]
	}
	w.WriteHeader(status)
}

func (h *hookRecorder) received() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.bodies...)
}

func TestNotifierDeliversDiscordPayload(t *testing.T) {
	hook := &hookRecorder{}
	ts := httptest.NewServer(hook)
	defer ts.Close()

	n := NewNotifier(AlertConfig{WebhookURL: ts.URL, WebhookFormat: "discord", CPUPercent: 80})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Check(Snapshot{Host: HostInfo{Hostname: "box1"}, CPU: CPUInfo{UsagePercent: 99}})
	waitFor(t, "webhook delivery", func() bool { return len(hook.received()) == 1 })

	got := hook.received()[0]
	content, _ := got["content"].(string)
	if !strings.HasPrefix(content, "[box1]\n") || !strings.Contains(content, "CPU usage is 99.0%") {
		t.Errorf("content = %q", content)
	}
	if _, ok := got["allowed_mentions"]; !ok {
		t.Error("discord payload should disable mentions")
	}
}

func TestNotifierRetriesThenSucceeds(t *testing.T) {
	old := alertBackoff
	alertBackoff = 10 * time.Millisecond
	defer func() { alertBackoff = old }()

	hook := &hookRecorder{statuses: []int{http.StatusBadGateway, http.StatusTooManyRequests}}
	ts := httptest.NewServer(hook)
	defer ts.Close()

	n := NewNotifier(AlertConfig{WebhookURL: ts.URL, MemoryPercent: 50})
	n.deliver(context.Background(), "hello")
	got := hook.received()
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3", len(got))
	}
	if got[2]["message"] != "hello" || got[2]["source"] != "statusserver" {
		t.Errorf("generic payload = %v", got[2])
	}
}

func TestNotifierNoRetryOnClientError(t *testing.T) {
	hook := &hookRecorder{statuses: []int{http.StatusNotFound}}
	ts := httptest.NewServer(hook)
	defer ts.Close()
	n := NewNotifier(AlertConfig{WebhookURL: ts.URL, CPUPercent: 50})
	n.deliver(context.Background(), "x")
	if len(hook.received()) != 1 {
		t.Errorf("a 404 must not be retried, attempts = %d", len(hook.received()))
	}
}

func TestNotifierErrorsDoNotLeakWebhookURL(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "http://127.0.0.1:1/api/webhooks/SECRET", CPUPercent: 50})
	_, _, err := n.post(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the webhook secret: %v", err)
	}
}

func TestPayloadFormats(t *testing.T) {
	cases := map[string]string{"slack": "text", "discord": "content", "": "message", "generic": "message"}
	for format, key := range cases {
		n := &Notifier{cfg: AlertConfig{WebhookFormat: format}}
		var m map[string]any
		if err := json.Unmarshal(n.payload("hi"), &m); err != nil || m[key] != "hi" {
			t.Errorf("format %q: payload %v (err %v), want %q=hi", format, m, err, key)
		}
	}
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
