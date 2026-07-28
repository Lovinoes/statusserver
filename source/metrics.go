package main

import (
	"fmt"
	"io"
	"runtime"
	"strings"
)

// writeMetrics renders the snapshot in prometheus text format (0.0.4),
// hand-rolled to stay dependency-free. all metrics are prefixed "statusserver_".
func writeMetrics(w io.Writer, snap Snapshot, wsClients int) {
	// build info is always emitted, even before the first snapshot.
	writeHelp(w, "statusserver_build_info", "gauge",
		"Build information; constant 1, labels carry version/commit.")
	fmt.Fprintf(w, "statusserver_build_info{version=\"%s\",commit=\"%s\",go=\"%s\"} 1\n",
		esc(version), esc(commit), esc(runtime.Version()))

	writeHelp(w, "statusserver_ws_clients", "gauge",
		"Number of connected websocket clients.")
	fmt.Fprintf(w, "statusserver_ws_clients %d\n", wsClients)

	// before the first snapshot, skip host metrics rather than emit zeros.
	if snap.Timestamp.IsZero() {
		return
	}

	writeHelp(w, "statusserver_scrape_time_seconds", "gauge",
		"Unix timestamp of the snapshot being exported.")
	fmt.Fprintf(w, "statusserver_scrape_time_seconds %d\n", snap.Timestamp.Unix())

	writeHelp(w, "statusserver_cpu_usage_percent", "gauge",
		"Overall CPU utilization, 0-100.")
	fmt.Fprintf(w, "statusserver_cpu_usage_percent %s\n", f(snap.CPU.UsagePercent))

	if snap.CPU.TemperatureC != nil {
		writeHelp(w, "statusserver_cpu_temperature_celsius", "gauge",
			"CPU temperature in degrees Celsius.")
		fmt.Fprintf(w, "statusserver_cpu_temperature_celsius %s\n", f(*snap.CPU.TemperatureC))
	}

	writeHelp(w, "statusserver_memory_total_bytes", "gauge", "Total physical memory in bytes.")
	fmt.Fprintf(w, "statusserver_memory_total_bytes %d\n", snap.Memory.TotalBytes)
	writeHelp(w, "statusserver_memory_used_bytes", "gauge", "Used physical memory in bytes.")
	fmt.Fprintf(w, "statusserver_memory_used_bytes %d\n", snap.Memory.UsedBytes)
	writeHelp(w, "statusserver_memory_used_percent", "gauge", "Used physical memory, 0-100.")
	fmt.Fprintf(w, "statusserver_memory_used_percent %s\n", f(snap.Memory.UsedPercent))

	// storage, labeled by mount.
	if len(snap.Storage) > 0 {
		writeHelp(w, "statusserver_disk_total_bytes", "gauge", "Total disk space in bytes.")
		for _, d := range snap.Storage {
			fmt.Fprintf(w, "statusserver_disk_total_bytes{mount=\"%s\",device=\"%s\",fstype=\"%s\"} %d\n",
				esc(d.Mount), esc(d.Device), esc(d.Fstype), d.TotalBytes)
		}
		writeHelp(w, "statusserver_disk_used_bytes", "gauge", "Used disk space in bytes.")
		for _, d := range snap.Storage {
			fmt.Fprintf(w, "statusserver_disk_used_bytes{mount=\"%s\",device=\"%s\",fstype=\"%s\"} %d\n",
				esc(d.Mount), esc(d.Device), esc(d.Fstype), d.UsedBytes)
		}
		writeHelp(w, "statusserver_disk_used_percent", "gauge", "Used disk space, 0-100.")
		for _, d := range snap.Storage {
			fmt.Fprintf(w, "statusserver_disk_used_percent{mount=\"%s\",device=\"%s\",fstype=\"%s\"} %s\n",
				esc(d.Mount), esc(d.Device), esc(d.Fstype), f(d.UsedPercent))
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

// f formats a float trimming trailing zeros.
func f(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

// esc escapes a Prometheus label value (backslash, double-quote, newline).
func esc(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
