package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	dayLayout = "2006-01-02"
	daySecs   = 86400

	// keepDays bounds the stored history; comfortably above the 365d window.
	keepDays = 400

	// uptimeSaveEvery bounds how often the history is written to disk. A
	// crash loses at most this much; a clean shutdown flushes everything.
	uptimeSaveEvery = time.Minute
)

// UptimeStore tracks, per UTC day, how many seconds the agent was alive, and
// persists that to JSON so history survives restarts.
//
// Time is credited from the gap between consecutive heartbeats, so a stall
// or a restart shows up as downtime instead of being papered over.
type UptimeStore struct {
	mu   sync.Mutex
	path string

	FirstSeen     time.Time          `json:"first_seen"`
	Days          map[string]float64 `json:"days"` // YYYY-MM-DD (UTC) -> seconds alive, max 86400
	LastHeartbeat time.Time          `json:"last_heartbeat"`

	lastBeat time.Time // in-process only; carries the monotonic clock
	dirty    bool
	lastSave time.Time
}

// loadUptimeStore reads the history at path. A missing file starts fresh; an
// unreadable one is moved aside (not silently overwritten) and starts fresh.
func loadUptimeStore(path string) *UptimeStore {
	s := &UptimeStore{path: path, Days: map[string]float64{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("WARNING: uptime history %s unreadable, starting fresh: %v", path, err)
		}
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		bad := path + ".corrupt"
		log.Printf("WARNING: uptime history %s is corrupt (%v); moved to %s, starting fresh", path, err, bad)
		_ = os.Rename(path, bad)
		s.FirstSeen, s.LastHeartbeat = time.Time{}, time.Time{}
		s.Days = map[string]float64{}
	}
	if s.Days == nil {
		s.Days = map[string]float64{}
	}
	return s
}

// Heartbeat marks the agent alive at now and credits the time since the
// previous heartbeat, capped at maxGap so a suspended process isn't credited
// for time it wasn't reporting. The first heartbeat only starts the clock.
func (s *UptimeStore) Heartbeat(now time.Time, maxGap time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wall := now.UTC()
	if s.FirstSeen.IsZero() {
		s.FirstSeen = wall
	}
	if !s.lastBeat.IsZero() {
		d := now.Sub(s.lastBeat) // monotonic, immune to wall clock jumps
		d = min(d, maxGap)
		if d > 0 {
			s.creditLocked(wall.Add(-d), wall)
		}
	}
	s.lastBeat = now
	s.LastHeartbeat = wall
	s.dirty = true
	s.pruneLocked(wall)
}

// creditLocked adds [from, to) to the per-day totals, splitting it at UTC
// midnight so each day gets its own share. Caller must hold s.mu.
func (s *UptimeStore) creditLocked(from, to time.Time) {
	for from.Before(to) {
		dayStart := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
		end := dayStart.AddDate(0, 0, 1)
		if to.Before(end) {
			end = to
		}
		key := from.Format(dayLayout)
		s.Days[key] = min(s.Days[key]+end.Sub(from).Seconds(), daySecs)
		from = end
	}
}

// pruneLocked drops history older than keepDays. Caller must hold s.mu.
func (s *UptimeStore) pruneLocked(now time.Time) {
	if len(s.Days) <= keepDays {
		return
	}
	cutoff := now.AddDate(0, 0, -keepDays).Format(dayLayout)
	for d := range s.Days {
		if d < cutoff {
			delete(s.Days, d)
		}
	}
}

// SaveIfDue persists the history if it changed and the last save was at
// least uptimeSaveEvery ago.
func (s *UptimeStore) SaveIfDue(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty || (!s.lastSave.IsZero() && now.Sub(s.lastSave) < uptimeSaveEvery) {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.lastSave = now
	return nil
}

// Flush persists any unsaved history; called on shutdown.
func (s *UptimeStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

// saveLocked writes the history atomically (temp file + fsync + rename) so a
// crash mid-write never leaves a corrupt file. Caller must hold s.mu.
func (s *UptimeStore) saveLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.dirty = false
	return nil
}

// Percent returns the share of the trailing n days (today included) the
// agent was alive. Each day's window is clipped to [FirstSeen, now], so a
// fresh install shows 100% and the first/current days are prorated rather
// than charged a full 24h.
func (s *UptimeStore) Percent(n int, now time.Time) float64 {
	if n <= 0 {
		return 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.FirstSeen.IsZero() {
		return 100
	}

	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var totalUp, totalExpected float64

	for i := 0; i < n; i++ {
		dayStart := today.AddDate(0, 0, -i)
		start := dayStart
		if s.FirstSeen.After(start) {
			start = s.FirstSeen
		}
		end := dayStart.AddDate(0, 0, 1)
		if now.Before(end) {
			end = now
		}

		expected := end.Sub(start).Seconds()
		if expected <= 0 {
			continue
		}
		totalUp += min(s.Days[dayStart.Format(dayLayout)], expected)
		totalExpected += expected
	}

	if totalExpected <= 0 {
		return 100
	}
	return totalUp / totalExpected * 100
}
