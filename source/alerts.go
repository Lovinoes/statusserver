package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Notifier posts a webhook message when a monitored metric crosses its
// threshold. Alerts are edge-triggered: one message on OK->breach, one on
// recovery, avoiding per-interval spam.
type Notifier struct {
	cfg    AlertConfig
	client *http.Client

	mu      sync.Mutex
	firing  map[string]bool // alert key -> in breach
	enabled bool
}

// NewNotifier builds a Notifier. Inert if no webhook URL or thresholds set.
func NewNotifier(cfg AlertConfig) *Notifier {
	enabled := cfg.WebhookURL != "" &&
		(cfg.CPUPercent > 0 || cfg.MemoryPercent > 0 || cfg.DiskPercent > 0 || cfg.TempC > 0)
	if enabled {
		log.Printf("alerts enabled -> %s (format %q)", redactURL(cfg.WebhookURL), cmp.Or(cfg.WebhookFormat, "generic"))
	}
	return &Notifier{
		cfg:     cfg,
		client:  &http.Client{Timeout: 10 * time.Second},
		firing:  map[string]bool{},
		enabled: enabled,
	}
}

// redactURL strips the path, query, and userinfo so a webhook secret is never
// written to logs; only scheme + host remain.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	return u.Scheme + "://" + u.Host + "/[redacted]"
}

// Check evaluates a snapshot and dispatches transition notifications in the
// background, never blocking the collection loop.
func (n *Notifier) Check(ctx context.Context, snap Snapshot) {
	if !n.enabled {
		return
	}
	msgs := n.evaluate(snap)
	if len(msgs) == 0 {
		return
	}
	sort.Strings(msgs)
	body := strings.Join(msgs, "\n")
	debugf("alert transition(s) detected, posting %d message(s) to webhook", len(msgs))
	go n.post(ctx, body)
}

// evaluate compares the snapshot against thresholds, updates firing state, and
// returns messages for any OK<->breach transitions.
func (n *Notifier) evaluate(snap Snapshot) []string {
	type eval struct {
		key       string
		label     string
		value     float64
		threshold float64
		unit      string
	}
	var evals []eval

	if n.cfg.CPUPercent > 0 {
		evals = append(evals, eval{"cpu", "CPU usage", snap.CPU.UsagePercent, n.cfg.CPUPercent, "%"})
	}
	if n.cfg.MemoryPercent > 0 {
		evals = append(evals, eval{"memory", "Memory usage", snap.Memory.UsedPercent, n.cfg.MemoryPercent, "%"})
	}
	if n.cfg.TempC > 0 && snap.CPU.TemperatureC != nil {
		evals = append(evals, eval{"temp", "CPU temperature", *snap.CPU.TemperatureC, n.cfg.TempC, "\u00b0C"})
	}
	if n.cfg.DiskPercent > 0 {
		for _, d := range snap.Storage {
			evals = append(evals, eval{
				key:       "disk:" + d.Mount,
				label:     "Disk usage (" + d.Mount + ")",
				value:     d.UsedPercent,
				threshold: n.cfg.DiskPercent,
				unit:      "%",
			})
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	var msgs []string
	for _, e := range evals {
		breaching := e.value >= e.threshold
		was := n.firing[e.key]
		switch {
		case breaching && !was:
			n.firing[e.key] = true
			msgs = append(msgs, fmt.Sprintf("\U0001f534 ALERT: %s is %.1f%s (threshold %.1f%s)",
				e.label, e.value, e.unit, e.threshold, e.unit))
		case !breaching && was:
			delete(n.firing, e.key)
			msgs = append(msgs, fmt.Sprintf("\u2705 RECOVERED: %s back to %.1f%s (threshold %.1f%s)",
				e.label, e.value, e.unit, e.threshold, e.unit))
		}
	}
	return msgs
}

// post delivers a message to the configured webhook.
func (n *Notifier) post(ctx context.Context, message string) {
	payload, contentType := n.payload(message)

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		log.Println("alert request build error:", err)
		return
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := n.client.Do(req)
	if err != nil {
		log.Println("alert delivery error:", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("alert webhook returned %d", resp.StatusCode)
		return
	}
	debugf("alert webhook delivered ok (status %d)", resp.StatusCode)
}

func (n *Notifier) payload(message string) ([]byte, string) {
	switch strings.ToLower(n.cfg.WebhookFormat) {
	case "discord":
		b, _ := json.Marshal(map[string]string{"content": message})
		return b, "application/json"
	case "slack":
		b, _ := json.Marshal(map[string]string{"text": message})
		return b, "application/json"
	default:
		b, _ := json.Marshal(map[string]any{
			"message":   message,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"source":    "statusserver",
		})
		return b, "application/json"
	}
}
