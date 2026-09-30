package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newMemStore() *UptimeStore { return &UptimeStore{Days: map[string]float64{}} }

func TestUptimePercentFreshInstall(t *testing.T) {
	s := newMemStore()
	for _, n := range []int{0, 7, 14, 30, 365} {
		if got := s.Percent(n, time.Now()); got != 100 {
			t.Errorf("fresh install Percent(%d) = %v, want 100", n, got)
		}
	}
}

func TestHeartbeatCreditsElapsedTime(t *testing.T) {
	s := newMemStore()
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s.Heartbeat(t0, time.Minute) // starts the clock, credits nothing
	if got := s.Days["2026-03-10"]; got != 0 {
		t.Errorf("first heartbeat credited %v, want 0", got)
	}
	s.Heartbeat(t0.Add(5*time.Second), time.Minute)
	s.Heartbeat(t0.Add(9500*time.Millisecond), time.Minute)
	if got := s.Days["2026-03-10"]; math.Abs(got-9.5) > 1e-9 {
		t.Errorf("credited %v, want 9.5 (fractions must not be truncated)", got)
	}
	if !s.FirstSeen.Equal(t0) || !s.LastHeartbeat.Equal(t0.Add(9500*time.Millisecond)) {
		t.Errorf("FirstSeen=%v LastHeartbeat=%v", s.FirstSeen, s.LastHeartbeat)
	}
	if got := s.Percent(1, t0.Add(9500*time.Millisecond)); math.Abs(got-100) > 1e-9 {
		t.Errorf("Percent = %v, want 100", got)
	}
}

func TestHeartbeatCapsGap(t *testing.T) {
	s := newMemStore()
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s.Heartbeat(t0, 35*time.Second)
	s.Heartbeat(t0.Add(time.Hour), 35*time.Second) // e.g. host was suspended
	if got := s.Days["2026-03-10"]; got != 35 {
		t.Errorf("credited %v, want gap capped at 35", got)
	}
	// 35s up out of 3600s elapsed.
	want := 35.0 / 3600 * 100
	if got := s.Percent(1, t0.Add(time.Hour)); math.Abs(got-want) > 1e-9 {
		t.Errorf("Percent = %v, want %v", got, want)
	}
}

func TestHeartbeatSplitsAtMidnight(t *testing.T) {
	s := newMemStore()
	t0 := time.Date(2026, 3, 10, 23, 59, 58, 0, time.UTC)
	s.Heartbeat(t0, time.Minute)
	s.Heartbeat(t0.Add(5*time.Second), time.Minute)
	if a, b := s.Days["2026-03-10"], s.Days["2026-03-11"]; a != 2 || b != 3 {
		t.Errorf("split = %v / %v, want 2 / 3", a, b)
	}
}

func TestHeartbeatDayCap(t *testing.T) {
	s := newMemStore()
	s.Days["2026-03-10"] = daySecs - 1
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s.Heartbeat(t0, time.Minute)
	s.Heartbeat(t0.Add(30*time.Second), time.Minute)
	if got := s.Days["2026-03-10"]; got != daySecs {
		t.Errorf("day = %v, want capped at %d", got, daySecs)
	}
}

func TestUptimeRestartCountsAsDowntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uptime.json")
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	s := loadUptimeStore(path)
	s.Heartbeat(t0, time.Minute)
	s.Heartbeat(t0.Add(10*time.Minute), time.Minute) // credits 60s (capped)
	for i := 1; i <= 540; i++ {                      // then steady 1s beats
		s.Heartbeat(t0.Add(10*time.Minute+time.Duration(i)*time.Second), time.Minute)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// "restart" 10 minutes later: the downtime must not be credited.
	s2 := loadUptimeStore(path)
	restart := t0.Add(29 * time.Minute)
	s2.Heartbeat(restart, time.Minute)
	s2.Heartbeat(restart.Add(time.Minute), time.Minute)
	up := s2.Days["2026-03-10"]
	if want := 60.0 + 540 + 60; up != want {
		t.Errorf("credited %v after restart, want %v", up, want)
	}
	if !s2.FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen lost across restart: %v", s2.FirstSeen)
	}
}

func TestUptimePercentHalfDown(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s := newMemStore()
	s.FirstSeen = now.AddDate(0, 0, -3)
	s.Days["2026-03-10"] = 6 * 3600 // half of the 12h elapsed today
	s.Days["2026-03-09"] = 43200
	s.Days["2026-03-08"] = 43200
	if got := s.Percent(3, now); math.Abs(got-50) > 1e-9 {
		t.Errorf("Percent(3) = %v, want 50", got)
	}
}

func TestUptimePercentProratesFirstDay(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s := newMemStore()
	s.FirstSeen = time.Date(2026, 3, 9, 18, 0, 0, 0, time.UTC) // 6h on day one
	s.Days["2026-03-09"] = 6 * 3600
	s.Days["2026-03-10"] = 12 * 3600
	if got := s.Percent(30, now); math.Abs(got-100) > 1e-9 {
		t.Errorf("Percent(30) = %v, want 100 (days before FirstSeen don't count)", got)
	}
}

func TestUptimeSaveIfDueThrottles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "uptime.json")
	s := loadUptimeStore(path)
	t0 := time.Now()
	s.Heartbeat(t0, time.Minute)
	if err := s.SaveIfDue(t0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first save should create the file (and its dirs): %v", err)
	}
	s.Heartbeat(t0.Add(5*time.Second), time.Minute)
	if err := s.SaveIfDue(t0.Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if !s.dirty {
		t.Error("save within uptimeSaveEvery should have been skipped")
	}
	if err := s.SaveIfDue(t0.Add(uptimeSaveEvery)); err != nil {
		t.Fatal(err)
	}
	if s.dirty {
		t.Error("save after uptimeSaveEvery should have happened")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestUptimeCorruptFileMovedAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uptime.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadUptimeStore(path)
	if !s.FirstSeen.IsZero() || len(s.Days) != 0 {
		t.Error("corrupt file should start fresh")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("corrupt file should be kept as .corrupt: %v", err)
	}
}

func TestUptimeLoadsLegacyIntegerDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uptime.json")
	legacy := `{"first_seen":"2026-01-01T00:00:00Z","days":{"2026-01-01":86400,"2026-01-02":120},"last_heartbeat":"2026-01-02T00:02:00Z"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadUptimeStore(path)
	if s.Days["2026-01-01"] != 86400 || s.Days["2026-01-02"] != 120 {
		t.Errorf("legacy days not loaded: %v", s.Days)
	}
}

func TestUptimePruneKeepsRecent(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		s.Days[now.AddDate(0, 0, -i).Format(dayLayout)] = 1
	}
	s.pruneLocked(now)
	if len(s.Days) > keepDays+1 {
		t.Errorf("after prune len = %d, want <= %d", len(s.Days), keepDays+1)
	}
	if _, ok := s.Days[now.AddDate(0, 0, -365).Format(dayLayout)]; !ok {
		t.Error("days inside the 365d window must survive pruning")
	}
}
