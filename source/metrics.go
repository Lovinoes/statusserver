package main

import (
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
)

// writeMetrics renders the snapshot in prometheus text format (0.0.4),
// hand-rolled to stay dependency-free. all metrics are prefixed "statusserver_".
// snap may be nil before the first collection.
func writeMetrics(w io.Writer, snap *Snapshot, wsClients int) {
	// build info is always emitted, even before the first snapshot.
	writeHelp(w, "statusserver_build_info", "gauge",
		"Build information; constant 1, labels carry version/commit.")
	fmt.Fprintf(w, "statusserver_build_info{version=\"%s\",commit=\"%s\",go=\"%s\"} 1\n",
		esc(version), esc(commit), esc(runtime.Version()))

	writeHelp(w, "statusserver_ws_clients", "gauge",
		"Number of connected websocket clients.")
	fmt.Fprintf(w, "statusserver_ws_clients %d\n", wsClients)

	// before the first snapshot, skip host metrics rather than emit zeros.
	if snap == nil {
		return
	}

	writeHelp(w, "statusserver_scrape_time_seconds", "gauge",
		"Unix timestamp of the snapshot being exported.")
	fmt.Fprintf(w, "statusserver_scrape_time_seconds %d\n", snap.Timestamp.Unix())

	writeHelp(w, "statusserver_host_info", "gauge",
		"Host identification; constant 1, labels carry the details.")
	fmt.Fprintf(w, "statusserver_host_info{hostname=\"%s\",os=\"%s\",platform=\"%s\",platform_version=\"%s\",kernel_version=\"%s\",arch=\"%s\"} 1\n",
		esc(snap.Host.Hostname), esc(snap.Host.OS), esc(snap.Host.Platform),
		esc(snap.Host.PlatformVersion), esc(snap.Host.KernelVersion), esc(snap.Host.Arch))

	writeHelp(w, "statusserver_cpu_usage_percent", "gauge",
		"Overall CPU utilization, 0-100.")
	fmt.Fprintf(w, "statusserver_cpu_usage_percent %s\n", f(snap.CPU.UsagePercent))

	writeHelp(w, "statusserver_cpu_cores", "gauge", "Number of logical CPU cores.")
	fmt.Fprintf(w, "statusserver_cpu_cores %d\n", snap.CPU.Cores)

	if snap.CPU.TemperatureC != nil {
		writeHelp(w, "statusserver_cpu_temperature_celsius", "gauge",
			"CPU temperature in degrees Celsius.")
		fmt.Fprintf(w, "statusserver_cpu_temperature_celsius %s\n", f(*snap.CPU.TemperatureC))
	}

	if l := snap.CPU.Load; l != nil {
		writeHelp(w, "statusserver_load_average", "gauge",
			"System load average, by window.")
		fmt.Fprintf(w, "statusserver_load_average{window=\"1m\"} %s\n", f(l.Load1))
		fmt.Fprintf(w, "statusserver_load_average{window=\"5m\"} %s\n", f(l.Load5))
		fmt.Fprintf(w, "statusserver_load_average{window=\"15m\"} %s\n", f(l.Load15))
	}

	writeHelp(w, "statusserver_memory_total_bytes", "gauge", "Total physical memory in bytes.")
	fmt.Fprintf(w, "statusserver_memory_total_bytes %d\n", snap.Memory.TotalBytes)
	writeHelp(w, "statusserver_memory_used_bytes", "gauge", "Used physical memory in bytes.")
	fmt.Fprintf(w, "statusserver_memory_used_bytes %d\n", snap.Memory.UsedBytes)
	writeHelp(w, "statusserver_memory_used_percent", "gauge", "Used physical memory, 0-100.")
	fmt.Fprintf(w, "statusserver_memory_used_percent %s\n", f(snap.Memory.UsedPercent))

	// storage, labeled by mount.
	if len(snap.Storage) > 0 {
		labels := func(d StorageInfo) string {
			return fmt.Sprintf("mount=\"%s\",device=\"%s\",fstype=\"%s\"", esc(d.Mount), esc(d.Device), esc(d.Fstype))
		}
		writeHelp(w, "statusserver_disk_total_bytes", "gauge", "Total disk space in bytes.")
		for _, d := range snap.Storage {
			fmt.Fprintf(w, "statusserver_disk_total_bytes{%s} %d\n", labels(d), d.TotalBytes)
		}
		writeHelp(w, "statusserver_disk_used_bytes", "gauge", "Used disk space in bytes.")
		for _, d := range snap.Storage {
			fmt.Fprintf(w, "statusserver_disk_used_bytes{%s} %d\n", labels(d), d.UsedBytes)
		}
		writeHelp(w, "statusserver_disk_used_percent", "gauge", "Used disk space, 0-100.")
		for _, d := range snap.Storage {
			fmt.Fprintf(w, "statusserver_disk_used_percent{%s} %s\n", labels(d), f(d.UsedPercent))
		}
	}

	// network, labeled by interface.
	if len(snap.Network) > 0 {
		writeHelp(w, "statusserver_network_rx_bytes_per_second", "gauge",
			"Receive throughput in bytes per second.")
		for _, n := range snap.Network {
			fmt.Fprintf(w, "statusserver_network_rx_bytes_per_second{interface=\"%s\"} %s\n",
				esc(n.Interface), f(n.RxBytesPerSec))
		}
		writeHelp(w, "statusserver_network_tx_bytes_per_second", "gauge",
			"Transmit throughput in bytes per second.")
		for _, n := range snap.Network {
			fmt.Fprintf(w, "statusserver_network_tx_bytes_per_second{interface=\"%s\"} %s\n",
				esc(n.Interface), f(n.TxBytesPerSec))
		}
		writeHelp(w, "statusserver_network_rx_bytes_total", "counter",
			"Cumulative bytes received since boot.")
		for _, n := range snap.Network {
			fmt.Fprintf(w, "statusserver_network_rx_bytes_total{interface=\"%s\"} %d\n",
				esc(n.Interface), n.RxTotalBytes)
		}
		writeHelp(w, "statusserver_network_tx_bytes_total", "counter",
			"Cumulative bytes transmitted since boot.")
		for _, n := range snap.Network {
			fmt.Fprintf(w, "statusserver_network_tx_bytes_total{interface=\"%s\"} %d\n",
				esc(n.Interface), n.TxTotalBytes)
		}
	}

	writeHelp(w, "statusserver_host_uptime_seconds", "gauge", "Host uptime in seconds since boot.")
	fmt.Fprintf(w, "statusserver_host_uptime_seconds %d\n", snap.Uptime.Seconds)

	writeHelp(w, "statusserver_agent_uptime_percent", "gauge",
		"Percentage of the trailing window the agent has been reporting, by window.")
	fmt.Fprintf(w, "statusserver_agent_uptime_percent{window=\"7d\"} %s\n", f(snap.Uptime.Percent7d))
	fmt.Fprintf(w, "statusserver_agent_uptime_percent{window=\"14d\"} %s\n", f(snap.Uptime.Percent14d))
	fmt.Fprintf(w, "statusserver_agent_uptime_percent{window=\"30d\"} %s\n", f(snap.Uptime.Percent30d))
	fmt.Fprintf(w, "statusserver_agent_uptime_percent{window=\"365d\"} %s\n", f(snap.Uptime.Percent365d))
}

// writeHelp emits the # HELP and # TYPE lines for a metric.
func writeHelp(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
}

// f formats a sample value with the shortest exact representation.
func f(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// esc escapes a Prometheus label value (backslash, double-quote, newline).
func esc(s string) string { return labelEscaper.Replace(s) }
