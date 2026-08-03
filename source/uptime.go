package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// UptimeStore tracks, per UTC day, seconds the agent was alive, persisted to
// JSON so history survives restarts.
type UptimeStore struct {
	mu   sync.Mutex
	path string

	FirstSeen     time.Time      `json:"first_seen"`
	Days          map[string]int `json:"days"` // YYYY-MM-DD -> seconds alive (max 86400)
	LastHeartbeat time.Time      `json:"last_heartbeat"`
}

func loadUptimeStore(path string) *UptimeStore {
	s := &UptimeStore{path: path, Days: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	_ = json.NewDecoder(f).Decode(s) // corrupt file just starts fresh
	if s.Days == nil {
		s.Days = map[string]int{}
	}
	return s
}

func (s *UptimeStore) save() error {
	if s.path == "" {
		return nil
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(s); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// atomic rename: a crash mid-write never leaves a corrupt state file.
	return os.Rename(tmp, s.path)
}

// Flush persists current state, called on graceful shutdown.
func (s *UptimeStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

func (s *UptimeStore) heartbeat(intervalSeconds int) error {
	if intervalSeconds <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	if s.FirstSeen.IsZero() {
		s.FirstSeen = now
	}

	s.Days[day] += intervalSeconds
	if s.Days[day] > 86400 {
		s.Days[day] = 86400
	}
	s.LastHeartbeat = now

	s.pruneLocked()
	return s.save()
}

// pruneLocked caps stored history at ~400 days. Caller must hold s.mu.
func (s *UptimeStore) pruneLocked() {
	if len(s.Days) <= 400 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -380).Format("2006-01-02")
	for d := range s.Days {
		if d < cutoff {
			delete(s.Days, d)
		}
	}
}

// percent returns the uptime percentage over the last n days. Each day's
// window is clipped to [FirstSeen, now], so a fresh install shows 100% and
// the first/current days are prorated rather than charged a full 24h.
func (s *UptimeStore) percent(n int) float64 {
	if n <= 0 {
		return 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.FirstSeen.IsZero() {
		return 100
	}

	now := time.Now().UTC()
	var totalUp, totalExpected float64

	for i := 0; i < n; i++ {
		day := now.AddDate(0, 0, -i)
		dayStr := day.Format("2006-01-02")
		dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		dayEnd := dayStart.Add(24 * time.Hour)

		start := dayStart
		if s.FirstSeen.After(start) {
			start = s.FirstSeen
		}
		end := dayEnd
		if now.Before(end) {
			end = now
		}

		expected := end.Sub(start).Seconds()
		if expected <= 0 {
			continue
		}

		up := float64(s.Days[dayStr])
		if up > expected {
			up = expected
		}
		totalUp += up
		totalExpected += expected
	}

	if totalExpected <= 0 {
		return 100
	}
	return totalUp / totalExpected * 100
}
