package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testConfig() Config {
	cfg := defaultConfig()
	cfg.AuthToken = "tok"
	cfg.AllowedOrigins = []string{"https://status.example.com", "*.trusted.example"}
	return cfg
}

func testSnapshot(cpu float64) *Snapshot {
	return &Snapshot{
		Timestamp: time.Now().UTC(),
		CPU:       CPUInfo{UsagePercent: cpu},
		Storage:   []StorageInfo{},
		Network:   []NetworkInfo{},
	}
}

func publishTest(s *Server, snap *Snapshot) {
	data, err := json.Marshal(snap)
	if err != nil {
		panic(err)
	}
	s.publish(snap, data)
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStatusEndpoint(t *testing.T) {
	s := newServer(testConfig())
	h := s.routes()

	if rec := do(t, h, "GET", "/api/status", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	} else if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 should carry WWW-Authenticate")
	}
	if rec := do(t, h, "GET", "/api/status?token=nope", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rec.Code)
	}

	rec := do(t, h, "GET", "/api/status?token=tok", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before first snapshot: got %d, want 503", rec.Code)
	}

	publishTest(s, testSnapshot(42))
	for _, auth := range []map[string]string{
		{"Authorization": "Bearer tok"},
		{"Authorization": "bearer tok"},
		nil, // query token below
	} {
		target := "/api/status"
		if auth == nil {
			target += "?token=tok"
		}
		rec := do(t, h, "GET", target, auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("auth %v: got %d, want 200", auth, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var got Snapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.CPU.UsagePercent != 42 {
			t.Errorf("body = %s (err %v)", rec.Body, err)
		}
	}

	if rec := do(t, h, "POST", "/api/status?token=tok", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d, want 405", rec.Code)
	}
}

func TestStatusEmptyArraysNotNull(t *testing.T) {
	s := newServer(defaultConfig())
	publishTest(s, testSnapshot(1))
	body := do(t, s.routes(), "GET", "/api/status", nil).Body.String()
	if !strings.Contains(body, `"storage":[]`) || !strings.Contains(body, `"network":[]`) {
		t.Errorf("storage/network should be [] not null: %s", body)
	}
}

func TestStatusCORS(t *testing.T) {
	s := newServer(testConfig())
	h := s.routes()

	cases := []struct {
		origin string
		allow  bool
	}{
		{"https://status.example.com", true},
		{"http://status.example.com", false}, // scheme is part of the pattern
		{"https://a.trusted.example", true},  // host glob
		{"http://b.trusted.example:8080", false},
		{"https://evil.example", false},
	}
	for _, c := range cases {
		rec := do(t, h, "OPTIONS", "/api/status", map[string]string{"Origin": c.origin})
		if rec.Code != http.StatusNoContent {
			t.Errorf("preflight %s: got %d, want 204", c.origin, rec.Code)
		}
		got := rec.Header().Get("Access-Control-Allow-Origin")
		if c.allow && got != c.origin {
			t.Errorf("origin %s should be allowed, header = %q", c.origin, got)
		}
		if !c.allow && got != "" {
			t.Errorf("origin %s should be refused, header = %q", c.origin, got)
		}
		if rec.Header().Get("Vary") != "Origin" {
			t.Errorf("Vary should be Origin, got %q", rec.Header().Get("Vary"))
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	if !originAllowed([]string{"*"}, "https://anything.example:1234") {
		t.Error(`"*" should allow any origin`)
	}
	if originAllowed([]string{"*"}, "null") {
		t.Error("an origin without a host should not match")
	}
	if !originAllowed([]string{"HTTPS://Status.Example.com"}, "https://status.example.com") {
		t.Error("matching should be case-insensitive")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newServer(testConfig())
	h := s.routes()
	if rec := do(t, h, "GET", "/metrics", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}
	rec := do(t, h, "GET", "/metrics", map[string]string{"Authorization": "Bearer tok"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "cpu_usage_percent") {
		t.Errorf("before first snapshot: %d\n%s", rec.Code, rec.Body)
	}
	publishTest(s, testSnapshot(12.5))
	rec = do(t, h, "GET", "/metrics", map[string]string{"Authorization": "Bearer tok"})
	if !strings.Contains(rec.Body.String(), "statusserver_cpu_usage_percent 12.5\n") {
		t.Errorf("metrics missing cpu:\n%s", rec.Body)
	}
}

func TestProbesAndVersionAreOpen(t *testing.T) {
	s := newServer(testConfig())
	h := s.routes()

	if rec := do(t, h, "GET", "/healthz", nil); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz before snapshot: got %d, want 503", rec.Code)
	}
	publishTest(s, testSnapshot(1))
	if rec := do(t, h, "GET", "/readyz", nil); rec.Code != http.StatusOK {
		t.Errorf("readyz after snapshot: got %d, want 200", rec.Code)
	}
	stale := testSnapshot(1)
	stale.Timestamp = time.Now().Add(-time.Hour)
	publishTest(s, stale)
	if rec := do(t, h, "GET", "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with stale snapshot: got %d, want 503", rec.Code)
	}

	rec := do(t, h, "GET", "/version", nil)
	var v map[string]string
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v["version"] == "" {
		t.Errorf("version: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff header missing")
	}
	if rec := do(t, h, "GET", "/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: got %d, want 404", rec.Code)
	}
}

func TestAuthDisabledAllowsAll(t *testing.T) {
	s := newServer(defaultConfig())
	publishTest(s, testSnapshot(1))
	if rec := do(t, s.routes(), "GET", "/api/status", nil); rec.Code != http.StatusOK {
		t.Errorf("no token configured: got %d, want 200", rec.Code)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"BEARER abc":   "abc",
		"Basic abc":    "",
		"abc":          "",
		"":             "",
	}
	for in, want := range cases {
		if got := bearerToken(in); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteIP(t *testing.T) {
	cases := []struct {
		xff        []string
		remote     string
		trustProxy bool
		want       string
	}{
		{nil, "1.2.3.4:5555", false, "1.2.3.4"},
		{nil, "1.2.3.4:5555", true, "1.2.3.4"},
		{nil, "[2001:db8::1]:5555", false, "2001:db8::1"},
		// trusted proxy: the last hop is the one the proxy appended; the
		// first hop is whatever the client claimed.
		{[]string{"6.6.6.6, 9.9.9.9"}, "127.0.0.1:5555", true, "9.9.9.9"},
		{[]string{"8.8.8.8"}, "127.0.0.1:5555", true, "8.8.8.8"},
		{[]string{"6.6.6.6", "7.7.7.7"}, "127.0.0.1:5555", true, "7.7.7.7"},
		{[]string{"9.9.9.9:4711"}, "127.0.0.1:5555", true, "9.9.9.9"},
		{[]string{"::ffff:9.9.9.9"}, "127.0.0.1:5555", true, "9.9.9.9"},
		{[]string{"garbage"}, "127.0.0.1:5555", true, "127.0.0.1"},
		// untrusted: ignore spoofable xff, use transport address.
		{[]string{"9.9.9.9, 10.0.0.1"}, "1.2.3.4:5555", false, "1.2.3.4"},
	}
	for _, c := range cases {
		r := &http.Request{Header: http.Header{}, RemoteAddr: c.remote}
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := remoteIP(r, c.trustProxy); got != c.want {
			t.Errorf("remoteIP(xff=%q, remote=%q, trust=%v) = %q, want %q", c.xff, c.remote, c.trustProxy, got, c.want)
		}
	}
}

// --- websocket ---------------------------------------------------------------

func startWS(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	s := newServer(cfg)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() {
		s.beginShutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.hub.close(ctx)
		ts.Close()
	})
	return s, ts
}

func wsURL(ts *httptest.Server, query string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws" + query
}

func dialWS(t *testing.T, url string, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if origin != "" {
		opts.HTTPHeader.Set("Origin", origin)
	}
	return websocket.Dial(ctx, url, opts)
}

func readSnap(t *testing.T, c *websocket.Conn) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type = %v, want text", typ)
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("bad snapshot json: %v", err)
	}
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebsocketStream(t *testing.T) {
	cfg := testConfig()
	s, ts := startWS(t, cfg)
	publishTest(s, testSnapshot(10))

	if _, resp, err := dialWS(t, wsURL(ts, ""), ""); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial without token should get 401, got resp=%v err=%v", resp, err)
	}

	c, _, err := dialWS(t, wsURL(ts, "?token=tok"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	if got := readSnap(t, c); got.CPU.UsagePercent != 10 {
		t.Errorf("initial snapshot cpu = %v, want 10", got.CPU.UsagePercent)
	}
	publishTest(s, testSnapshot(20))
	if got := readSnap(t, c); got.CPU.UsagePercent != 20 {
		t.Errorf("pushed snapshot cpu = %v, want 20", got.CPU.UsagePercent)
	}
	waitFor(t, "client count 1", func() bool { return s.hub.count() == 1 })

	c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "client removal", func() bool { return s.hub.count() == 0 })
}

func TestWebsocketNoSnapshotYetWaits(t *testing.T) {
	s, ts := startWS(t, defaultConfig())
	c, _, err := dialWS(t, wsURL(ts, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	waitFor(t, "client registered", func() bool { return s.hub.count() == 1 })
	publishTest(s, testSnapshot(33))
	if got := readSnap(t, c); got.CPU.UsagePercent != 33 {
		t.Errorf("first snapshot cpu = %v, want 33", got.CPU.UsagePercent)
	}
}

func TestWebsocketOriginCheck(t *testing.T) {
	_, ts := startWS(t, testConfig())
	if _, resp, err := dialWS(t, wsURL(ts, "?token=tok"), "https://evil.example"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin should get 403, got resp=%v err=%v", resp, err)
	}
	c, _, err := dialWS(t, wsURL(ts, "?token=tok"), "https://status.example.com")
	if err != nil {
		t.Fatalf("allowed origin rejected: %v", err)
	}
	c.CloseNow()
}

func TestWebsocketPerIPLimit(t *testing.T) {
	cfg := defaultConfig()
	cfg.MaxConnsPerIP = 1
	s, ts := startWS(t, cfg)
	publishTest(s, testSnapshot(1))

	first, _, err := dialWS(t, wsURL(ts, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseNow()
	readSnap(t, first)

	second, _, err := dialWS(t, wsURL(ts, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = second.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusTryAgainLater {
		t.Errorf("second connection close status = %v (err %v), want 1013", got, err)
	}
	if n := s.hub.count(); n != 1 {
		t.Errorf("hub count = %d, want 1", n)
	}
}

func TestWebsocketShutdownSendsGoingAway(t *testing.T) {
	s, ts := startWS(t, defaultConfig())
	publishTest(s, testSnapshot(1))
	c, _, err := dialWS(t, wsURL(ts, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readSnap(t, c)

	// the client must keep reading for the close handshake to complete.
	readErr := make(chan error, 1)
	go func() {
		_, _, err := c.Read(context.Background())
		readErr <- err
	}()

	s.beginShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.hub.close(ctx); err != nil {
		t.Fatalf("hub close: %v", err)
	}
	select {
	case err := <-readErr:
		if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
			t.Errorf("close status = %v (err %v), want 1001", got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never saw the close")
	}

	// new connections are refused once shutting down.
	c2, _, err := dialWS(t, wsURL(ts, ""), "")
	if err == nil {
		defer c2.CloseNow()
		_, _, err = c2.Read(ctx)
		if got := websocket.CloseStatus(err); got != websocket.StatusTryAgainLater {
			t.Errorf("post-shutdown connection status = %v, want 1013", got)
		}
	}
}

func TestClientOfferKeepsNewest(t *testing.T) {
	c := &client{send: make(chan []byte, 1)}
	c.offer([]byte("1"))
	c.offer([]byte("2"))
	c.offer([]byte("3"))
	if got := string(<-c.send); got != "3" {
		t.Errorf("pending = %q, want newest (3)", got)
	}
	select {
	case extra := <-c.send:
		t.Errorf("unexpected extra message %q", extra)
	default:
	}
}

func TestHealthcheckURL(t *testing.T) {
	cases := []struct {
		addr string
		tls  bool
		want string
	}{
		{":8090", false, "http://127.0.0.1:8090/healthz"},
		{"0.0.0.0:9000", false, "http://127.0.0.1:9000/healthz"},
		{"[::]:9000", true, "https://127.0.0.1:9000/healthz"},
		{"localhost:1", false, "http://localhost:1/healthz"},
		{"[::1]:2", false, "http://[::1]:2/healthz"},
	}
	for _, c := range cases {
		cfg := Config{ListenAddr: c.addr}
		if c.tls {
			cfg.TLSCert, cfg.TLSKey = "c", "k"
		}
		got, err := healthcheckURL(cfg)
		if err != nil || got != c.want {
			t.Errorf("healthcheckURL(%q) = %q, %v; want %q", c.addr, got, err, c.want)
		}
	}
	if _, err := healthcheckURL(Config{ListenAddr: "nonsense"}); err == nil {
		t.Error("listen_addr without a port should error")
	}
}

func TestRunHealthcheck(t *testing.T) {
	s := newServer(defaultConfig())
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")
	if code := runHealthcheck(Config{ListenAddr: addr}); code != 0 {
		t.Errorf("healthy server: exit code %d, want 0", code)
	}
	ts.Close()
	if code := runHealthcheck(Config{ListenAddr: addr}); code != 1 {
		t.Errorf("closed server: exit code %d, want 1", code)
	}
}

func TestUnauthorizedBody(t *testing.T) {
	rec := httptest.NewRecorder()
	unauthorized(rec)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(string(body), "unauthorized") {
		t.Errorf("got %d %q", rec.Code, body)
	}
}
