package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// UptimeStore tracks, per calendar day (UTC), how many seconds the agent
// was alive and reporting. It's persisted to a small JSON file so the
// history survives restarts and reboots - without that, a 365-day
// percentage would be meaningless (it'd reset to "100%, 0 data" every
// time the service restarted).
type UptimeStore struct {
	mu   sync.Mutex
	path string

	FirstSeen     string         `json:"first_seen"` // YYYY-MM-DD, UTC - the day this install first ran
	Days          map[string]int `json:"days"`       // YYYY-MM-DD -> seconds recorded alive that day (max 86400)
	LastHeartbeat time.Time      `json:"last_heartbeat"`
}

func loadUptimeStore(path string) *UptimeStore {
	s := &UptimeStore{path: path, Days: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		return s // no file yet - fresh start, not an error
	}
	defer f.Close()
	_ = json.NewDecoder(f).Decode(s) // best effort: a corrupt file just starts fresh instead of crashing
	if s.Days == nil {
		s.Days = map[string]int{}
	}
	return s
}

func (s *UptimeStore) save() {
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	err = json.NewEncoder(f).Encode(s)
	f.Close()
	if err != nil {
		return
	}
	// Rename is atomic on the same filesystem, so a crash mid-write never
	// leaves a half-written, corrupt state file in place.
	os.Rename(tmp, s.path)
}

// heartbeat credits roughly intervalSeconds of uptime to today.
func (s *UptimeStore) heartbeat(intervalSeconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	if s.FirstSeen == "" {
		s.FirstSeen = day
	}

	s.Days[day] += intervalSeconds
	if s.Days[day] > 86400 {
		s.Days[day] = 86400
	}
	s.LastHeartbeat = now

	s.pruneLocked()
	s.save()
}

// pruneLocked keeps the file from growing forever - a bit over a year of
// daily entries is plenty for a 365-day window. Caller must hold s.mu.
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

// percent returns the uptime percentage over the last n days, including
// today prorated for however much of today has elapsed. Days before the
// agent's first recorded run are excluded from both sides of the ratio,
// so a fresh install correctly shows 100% rather than being penalized for
// days it didn't exist yet.
func (s *UptimeStore) percent(n int) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	var totalUp, totalExpected int

	for i := 0; i < n; i++ {
		day := now.AddDate(0, 0, -i)
		dayStr := day.Format("2006-01-02")
		if s.FirstSeen != "" && dayStr < s.FirstSeen {
			break
		}

		expected := 86400
		if i == 0 {
			midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
			expected = int(now.Sub(midnight).Seconds())
		}

		up := s.Days[dayStr]
		if up > expected {
			up = expected
		}
		totalUp += up
		totalExpected += expected
	}

	if totalExpected <= 0 {
		return 100 // no data yet (e.g. brand new install) - nothing to report as down
	}
	return float64(totalUp) / float64(totalExpected) * 100
}
