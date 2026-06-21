package main

import (
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

type Snapshot struct {
	Timestamp time.Time     `json:"timestamp"`
	CPU       CPUInfo       `json:"cpu"`
	Memory    MemoryInfo    `json:"memory"`
	Storage   []StorageInfo `json:"storage"`
	Network   []NetworkInfo `json:"network"`
	Uptime    UptimeInfo    `json:"uptime"`
}

type UptimeInfo struct {
	Seconds     uint64  `json:"seconds"`      // host uptime, seconds since boot
	Human       string  `json:"human"`        // e.g. "27d 1h 49m 9s"
	Percent7d   float64 `json:"percent_7d"`   // agent reliability over the last 7 days
	Percent14d  float64 `json:"percent_14d"`
	Percent30d  float64 `json:"percent_30d"`
	Percent365d float64 `json:"percent_365d"`
}

type CPUInfo struct {
	// nil if no temperature sensor could be read (common on VPS/cloud hosts).
	TemperatureC *float64 `json:"temperature_c"`
	UsagePercent float64  `json:"usage_percent"`
}

type MemoryInfo struct {
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
	TotalHuman  string  `json:"total_human"`
	UsedHuman   string  `json:"used_human"`
}

type StorageInfo struct {
	Mount       string  `json:"mount"`
	Device      string  `json:"device"`
	Fstype      string  `json:"fstype"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
	TotalHuman  string  `json:"total_human"`
	UsedHuman   string  `json:"used_human"`
}

type NetworkInfo struct {
	Interface     string   `json:"interface"`
	RxBytesPerSec float64  `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64  `json:"tx_bytes_per_sec"`
	RxHuman       string   `json:"rx_human"`
	TxHuman       string   `json:"tx_human"`
	RxTotalBytes  uint64   `json:"rx_total_bytes"`
	TxTotalBytes  uint64   `json:"tx_total_bytes"`
	MaxMbps       *float64 `json:"max_mbps,omitempty"`
}

// prevNet/prevNetTime hold the previous tick's raw counters so we can turn
// cumulative byte counts into a per-second rate.
var prevNet = map[string]net.IOCountersStat{}
var prevNetTime time.Time

func collect(cfg Config, uptime *UptimeStore) Snapshot {
	snap := Snapshot{Timestamp: time.Now()}

	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		snap.CPU.UsagePercent = pct[0]
	}

	if temps, err := host.SensorsTemperatures(); err == nil {
		for _, t := range temps {
			if cfg.TempSensorMatch == "" || strings.Contains(strings.ToLower(t.SensorKey), strings.ToLower(cfg.TempSensorMatch)) {
				v := t.Temperature
				snap.CPU.TemperatureC = &v
				break
			}
		}
	}

	if vm, err := mem.VirtualMemory(); err == nil {
		snap.Memory = MemoryInfo{
			TotalBytes:  vm.Total,
			UsedBytes:   vm.Used,
			UsedPercent: vm.UsedPercent,
			TotalHuman:  humanizeBytes(float64(vm.Total)),
			UsedHuman:   humanizeBytes(float64(vm.Used)),
		}
	}

	if parts, err := disk.Partitions(false); err == nil {
		for _, p := range parts {
			if len(cfg.Disks) > 0 && !contains(cfg.Disks, p.Mountpoint) {
				continue
			}
			usage, err := disk.Usage(p.Mountpoint)
			if err != nil {
				continue
			}
			snap.Storage = append(snap.Storage, StorageInfo{
				Mount:       p.Mountpoint,
				Device:      p.Device,
				Fstype:      p.Fstype,
				TotalBytes:  usage.Total,
				UsedBytes:   usage.Used,
				UsedPercent: usage.UsedPercent,
				TotalHuman:  humanizeBytes(float64(usage.Total)),
				UsedHuman:   humanizeBytes(float64(usage.Used)),
			})
		}
	}

	if counters, err := net.IOCounters(true); err == nil {
		now := time.Now()
		elapsed := now.Sub(prevNetTime).Seconds()
		if elapsed <= 0 {
			elapsed = float64(cfg.IntervalSeconds)
		}
		for _, c := range counters {
			if len(cfg.Networks) > 0 {
				if !contains(cfg.Networks, c.Name) {
					continue
				}
			} else if isLikelyVirtual(c.Name) {
				continue
			}

			var rxRate, txRate float64
			if prev, ok := prevNet[c.Name]; ok {
				// Guard against counter resets (interface restart, reboot)
				// which would otherwise underflow into a huge bogus rate.
				if c.BytesRecv >= prev.BytesRecv {
					rxRate = float64(c.BytesRecv-prev.BytesRecv) / elapsed
				}
				if c.BytesSent >= prev.BytesSent {
					txRate = float64(c.BytesSent-prev.BytesSent) / elapsed
				}
			}

			info := NetworkInfo{
				Interface:     c.Name,
				RxBytesPerSec: rxRate,
				TxBytesPerSec: txRate,
				RxHuman:       humanizeBytes(rxRate) + "/s",
				TxHuman:       humanizeBytes(txRate) + "/s",
				RxTotalBytes:  c.BytesRecv,
				TxTotalBytes:  c.BytesSent,
			}
			if max, ok := cfg.NetworkMaxMbps[c.Name]; ok {
				info.MaxMbps = &max
			}
			snap.Network = append(snap.Network, info)
		}
		for _, c := range counters {
			prevNet[c.Name] = c
		}
		prevNetTime = now
	}

	if secs, err := host.Uptime(); err == nil {
		snap.Uptime.Seconds = secs
		snap.Uptime.Human = humanizeDuration(secs)
	}
	snap.Uptime.Percent7d = uptime.percent(7)
	snap.Uptime.Percent14d = uptime.percent(14)
	snap.Uptime.Percent30d = uptime.percent(30)
	snap.Uptime.Percent365d = uptime.percent(365)

	return snap
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}

func isLikelyVirtual(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"lo", "docker", "veth", "br-", "virbr", "tun", "tap"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
