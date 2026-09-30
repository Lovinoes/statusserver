package main

import (
	"math"
	"testing"
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
