package main

import "fmt"

// humanizeBytes picks the smallest unit that keeps the number under 1024,
// so 40 MiB stays "40.00 MiB" and 2100 MiB becomes "2.05 GiB" automatically.
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
