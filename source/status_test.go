package main

import (
	"math"
	"testing"
	"time"
)

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00 B"},
		{-5, "0.00 B"}, // negatives are clamped to zero
		{512, "512.00 B"},
		{1024, "1.00 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1536 * 1024, "1.50 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
		{1024.0 * 1024 * 1024 * 1024, "1.00 TiB"},
		{1024.0 * 1024 * 1024 * 1024 * 1024, "1.00 PiB"},
		// beyond the largest unit it stays in PiB rather than overflowing.
		{1024.0 * 1024 * 1024 * 1024 * 1024 * 1024, "1024.00 PiB"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanizeDuration(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0s"},
		{9, "9s"},
		{69, "1m 9s"},
		{3661, "1h 1m 1s"},
		{90000, "1d 1h 0m 0s"},
		{2374149, "27d 11h 29m 9s"},
	}
	for _, c := range cases {
		if got := humanizeDuration(c.in); got != c.want {
			t.Errorf("humanizeDuration(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRoundPct(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{100, 100},
		{99.999, 100},
		{99.994, 99.99},
		{99.955, 99.96},
		{0, 0},
	}
	for _, c := range cases {
		if got := roundPct(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("roundPct(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsLikelyVirtual(t *testing.T) {
	virtual := []string{"lo", "docker0", "veth1234", "br-abcdef", "virbr0", "tun0", "tap0"}
	for _, n := range virtual {
		if !isLikelyVirtual(n) {
			t.Errorf("expected %q to be treated as virtual", n)
		}
	}
	real := []string{"eth0", "enp3s0", "wlan0", "ens18"}
	for _, n := range real {
		if isLikelyVirtual(n) {
			t.Errorf("did not expect %q to be treated as virtual", n)
		}
	}
}

func TestUptimePercentFreshInstall(t *testing.T) {
	// a store with no history at all should report 100% - nothing to
	// penalize a brand new install for.
	s := &UptimeStore{Days: map[string]int{}}
	for _, n := range []int{7, 14, 30, 365} {
		if got := s.percent(n); got != 100 {
			t.Errorf("fresh install percent(%d) = %v, want 100", n, got)
		}
	}
}

func TestUptimePercentFullCoverage(t *testing.T) {
	// agent has existed for 10 full days and recorded a full 86400s each of
	// the last 7 completed days plus everything elapsed today. that should
	// come out very close to 100%.
	now := time.Now().UTC()
	s := &UptimeStore{
		Days:      map[string]int{},
		FirstSeen: now.AddDate(0, 0, -10),
	}
	for i := 0; i <= 10; i++ {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		s.Days[day] = 86400
	}
	got := s.percent(7)
	if got < 99.99 || got > 100.0001 {
		t.Errorf("percent(7) with full coverage = %v, want ~100", got)
	}
}

func TestUptimePercentHalfDown(t *testing.T) {
	// exactly half of every day in the window is recorded as up, including
	// the partial slice of today. that must come out to ~50% no matter what
	// time of day the test runs (the prorated "today" bucket otherwise grows
	// through the day and skews a fixed value).
	now := time.Now().UTC()
	s := &UptimeStore{
		Days:      map[string]int{},
		FirstSeen: now.AddDate(0, 0, -3),
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	elapsedToday := int(now.Sub(midnight).Seconds())

	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	dayBefore := now.AddDate(0, 0, -2).Format("2006-01-02")
	s.Days[today] = elapsedToday / 2
	s.Days[yesterday] = 43200
	s.Days[dayBefore] = 43200

	got := s.percent(3)
	if got < 49 || got > 51 {
		t.Errorf("percent(3) half-down = %v, want roughly 50", got)
	}
}

func TestUptimeHeartbeatAccumulates(t *testing.T) {
	s := &UptimeStore{Days: map[string]int{}} // no path => in-memory only
	if err := s.heartbeat(5); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := s.heartbeat(5); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if s.Days[day] != 10 {
		t.Errorf("after two 5s heartbeats, today = %d, want 10", s.Days[day])
	}
	if s.FirstSeen.IsZero() {
		t.Error("FirstSeen should be set after first heartbeat")
	}
}

func TestUptimeHeartbeatCapsAtDay(t *testing.T) {
	s := &UptimeStore{Days: map[string]int{}}
	// a single absurdly long interval must never exceed a full day.
	if err := s.heartbeat(100000); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if s.Days[day] != 86400 {
		t.Errorf("day should be capped at 86400, got %d", s.Days[day])
	}
}

func TestUptimeStorePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/uptime.json"

	s := loadUptimeStore(path)
	if err := s.heartbeat(5); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// reload from disk and confirm the counters survived.
	s2 := loadUptimeStore(path)
	day := time.Now().UTC().Format("2006-01-02")
	if s2.Days[day] != 5 {
		t.Errorf("reloaded day = %d, want 5", s2.Days[day])
	}
	if s2.FirstSeen.IsZero() {
		t.Error("reloaded FirstSeen should be set")
	}
}

func TestUptimePruneKeepsRecent(t *testing.T) {
	s := &UptimeStore{Days: map[string]int{}}
	// seed 410 days of history to trigger pruning.
	base := time.Now().UTC()
	for i := 0; i < 410; i++ {
		day := base.AddDate(0, 0, -i).Format("2006-01-02")
		s.Days[day] = 86400
	}
	s.pruneLocked()
	if len(s.Days) > 400 {
		t.Errorf("after prune, len(Days) = %d, want <= 400", len(s.Days))
	}
	// today must always survive pruning.
	if _, ok := s.Days[base.Format("2006-01-02")]; !ok {
		t.Error("today should not be pruned")
	}
}
