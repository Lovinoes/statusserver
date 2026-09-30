package main

import (
	"strings"
	"testing"
	"time"
)

func TestWriteMetricsContents(t *testing.T) {
	temp := 55.5
	snap := &Snapshot{
		Timestamp: time.Unix(1700000000, 0),
		Host:      HostInfo{Hostname: "box", OS: "linux", Arch: "amd64"},
		CPU:       CPUInfo{UsagePercent: 42.5, TemperatureC: &temp, Cores: 8, Load: &LoadAvg{Load1: 0.5, Load5: 1, Load15: 1.25}},
		Memory:    MemoryInfo{TotalBytes: 1000, UsedBytes: 500, UsedPercent: 50},
		Storage:   []StorageInfo{{Mount: `C:\`, Device: "C:", Fstype: "NTFS", UsedPercent: 73.2, TotalBytes: 100, UsedBytes: 73}},
		Network:   []NetworkInfo{{Interface: "eth0", RxBytesPerSec: 12345678.5, TxBytesPerSec: 8, RxTotalBytes: 100, TxTotalBytes: 200}},
		Uptime:    UptimeInfo{Seconds: 3600, Percent7d: 99.9},
	}
	var sb strings.Builder
	writeMetrics(&sb, snap, 2)
	out := sb.String()

	wants := []string{
		"statusserver_build_info{version=",
		"statusserver_ws_clients 2\n",
		"statusserver_scrape_time_seconds 1700000000\n",
		`statusserver_host_info{hostname="box",os="linux",platform="",platform_version="",kernel_version="",arch="amd64"} 1`,
		"statusserver_cpu_usage_percent 42.5\n",
		"statusserver_cpu_cores 8\n",
		"statusserver_cpu_temperature_celsius 55.5\n",
		`statusserver_load_average{window="1m"} 0.5`,
		`statusserver_load_average{window="15m"} 1.25`,
		"statusserver_memory_used_percent 50\n",
		`statusserver_disk_used_percent{mount="C:\\",device="C:",fstype="NTFS"} 73.2`,
		// no scientific notation / truncation for large rates.
		`statusserver_network_rx_bytes_per_second{interface="eth0"} 12345678.5`,
		`statusserver_network_tx_bytes_total{interface="eth0"} 200`,
		"statusserver_host_uptime_seconds 3600\n",
		`statusserver_agent_uptime_percent{window="7d"} 99.9`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("metrics output missing %q\n---\n%s", w, out)
		}
	}

	// every metric family gets exactly one HELP and TYPE line.
	seen := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			seen[strings.Fields(line)[2]]++
		}
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s has %d TYPE lines", name, n)
		}
	}
}

func TestWriteMetricsNilSnapshot(t *testing.T) {
	var sb strings.Builder
	writeMetrics(&sb, nil, 0)
	out := sb.String()
	if !strings.Contains(out, "build_info") || !strings.Contains(out, "ws_clients 0") {
		t.Error("build_info and ws_clients should always be present")
	}
	if strings.Contains(out, "cpu_usage_percent") {
		t.Error("host metrics should be omitted before the first snapshot")
	}
}

func TestWriteMetricsOmitsOptional(t *testing.T) {
	var sb strings.Builder
	writeMetrics(&sb, &Snapshot{Timestamp: time.Now()}, 0)
	out := sb.String()
	for _, absent := range []string{"cpu_temperature_celsius", "load_average", "disk_", "network_"} {
		if strings.Contains(out, absent) {
			t.Errorf("%s should be omitted when unavailable", absent)
		}
	}
}

func TestEscAndFormat(t *testing.T) {
	if got := esc("a\\b\"c\nd"); got != `a\\b\"c\nd` {
		t.Errorf("esc = %q", got)
	}
	cases := map[float64]string{0: "0", 100: "100", 0.00001: "0.00001", 42.5: "42.5", 1e9: "1000000000"}
	for in, want := range cases {
		if got := f(in); got != want {
			t.Errorf("f(%v) = %q, want %q", in, got, want)
		}
	}
}
