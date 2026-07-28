package main

import (
	"fmt"
	"math"
	"time"
)

func humanizeBytes(b float64) string {
	if b < 0 {
		b = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", b, units[i])
}

func humanizeDuration(totalSeconds uint64) string {
	d := time.Duration(totalSeconds) * time.Second
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	mins := int(d/time.Minute) % 60
	secs := int(d/time.Second) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, mins, secs)
	case hours > 0:
		return fmt.Sprintf("%dh %dm %ds", hours, mins, secs)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func roundPct(p float64) float64 {
	return math.Round(p*100) / 100
}
