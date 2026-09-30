package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	alertQueueSize   = 32
	alertAttempts    = 3
	alertPostTimeout = 10 * time.Second
	alertMaxBackoff  = time.Minute
)

// alertBackoff is the first retry delay (doubling after); a var for tests.
var alertBackoff = 2 * time.Second

// Notifier posts a webhook message when a monitored metric crosses its
// threshold. Alerts are edge-triggered: one message on OK->breach, one on
// recovery, avoiding per-interval spam. Messages go out one at a time, in
// order, from a single delivery goroutine (Run).
type Notifier struct {
	cfg     AlertConfig
	client  *http.Client
	enabled bool

	firing map[string]bool // alert key -> in breach; only touched by Check
	queue  chan string
}

// NewNotifier builds a Notifier. Inert if no webhook URL or thresholds set.
func NewNotifier(cfg AlertConfig) *Notifier {
	enabled := cfg.WebhookURL != "" && cfg.anyThreshold()
	if enabled {
		log.Printf("alerts enabled -> %s (format %q)", redactURL(cfg.WebhookURL), cmp.Or(cfg.WebhookFormat, "generic"))
	}
	return &Notifier{
		cfg:     cfg,
		client:  &http.Client{Timeout: alertPostTimeout},
		enabled: enabled,
		firing:  map[string]bool{},
		queue:   make(chan string, alertQueueSize),
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

// Check evaluates a snapshot and queues a message for any transitions. It
// never blocks the collection loop; if the queue is full the message is
// dropped (and logged).
func (n *Notifier) Check(snap Snapshot) {
	if !n.enabled {
		return
	}
	msgs := n.evaluate(snap)
	if len(msgs) == 0 {
		return
	}
	body := strings.Join(msgs, "\n")
	if h := snap.Host.Hostname; h != "" {
		body = "[" + h + "]\n" + body
	}
	debugf("alert transition(s) detected, queueing %d message(s)", len(msgs))
	select {
	case n.queue <- body:
	default:
		log.Printf("alert queue full, dropping alert: %s", strings.ReplaceAll(body, "\n", " | "))
	}
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
		evals = append(evals, eval{"temp", "CPU temperature", *snap.CPU.TemperatureC, n.cfg.TempC, "°C"})
	}
	if n.cfg.DiskPercent > 0 {
		for _, d := range snap.Storage {
			evals = append(evals, eval{"disk:" + d.Mount, "Disk usage (" + d.Mount + ")", d.UsedPercent, n.cfg.DiskPercent, "%"})
		}
	}

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
			msgs = append(msgs, fmt.Sprintf("✅ RECOVERED: %s back to %.1f%s (threshold %.1f%s)",
				e.label, e.value, e.unit, e.threshold, e.unit))
		}
	}
	sort.Strings(msgs)
	return msgs
}

// Run delivers queued messages until ctx is done.
func (n *Notifier) Run(ctx context.Context) {
	if !n.enabled {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-n.queue:
			n.deliver(ctx, msg)
		}
	}
}

// deliver posts one message, retrying network errors, 429s and 5xx with
// backoff (honoring Retry-After).
func (n *Notifier) deliver(ctx context.Context, message string) {
	payload := n.payload(message)
	backoff := alertBackoff
	for attempt := 1; ; attempt++ {
		retry, wait, err := n.post(ctx, payload)
		if err == nil {
			debugf("alert webhook delivered")
			return
		}
		if !retry || attempt >= alertAttempts || ctx.Err() != nil {
			log.Printf("alert delivery failed (attempt %d/%d): %v", attempt, alertAttempts, err)
			return
		}
		wait = min(max(wait, backoff), alertMaxBackoff)
		debugf("alert delivery attempt %d failed (%v), retrying in %s", attempt, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff *= 2
	}
}

// post sends one request. It reports whether a failure is worth retrying
// and how long the server asked us to wait.
func (n *Notifier) post(ctx context.Context, payload []byte) (retry bool, wait time.Duration, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, alertPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "statusserver/"+version)

	resp, err := n.client.Do(req)
	if err != nil {
		// the url may carry a secret; *url.Error includes it verbatim.
		return true, 0, fmt.Errorf("post to %s: %s", redactURL(n.cfg.WebhookURL), unwrapURLError(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // allow conn reuse

	if resp.StatusCode < 300 {
		return false, 0, nil
	}
	err = fmt.Errorf("webhook returned %s", resp.Status)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		if secs, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
		return true, wait, err
	}
	return false, 0, err
}

// unwrapURLError drops the request URL from a *url.Error message.
func unwrapURLError(err error) string {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err.Error()
	}
	return err.Error()
}

func (n *Notifier) payload(message string) []byte {
	var v any
	switch n.cfg.WebhookFormat {
	case "discord":
		// never let a hostname or mount path ping @everyone.
		v = map[string]any{"content": message, "allowed_mentions": map[string]any{"parse": []string{}}}
	case "slack":
		v = map[string]string{"text": message}
	default:
		v = map[string]string{
			"message":   message,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"source":    "statusserver",
		}
	}
	b, _ := json.Marshal(v) // plain maps of strings can't fail to marshal
	return b
}
