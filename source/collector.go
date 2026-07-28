package main

import (
	"log"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
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
	Seconds uint64 `json:"seconds"` // host uptime since boot
	Human   string `json:"human"`

	// Percent* reflect this agent's reporting reliability, not host uptime.
	Percent7d   float64 `json:"percent_7d"`
	Percent14d  float64 `json:"percent_14d"`
	Percent30d  float64 `json:"percent_30d"`
	Percent365d float64 `json:"percent_365d"`
}

type CPUInfo struct {
	TemperatureC *float64 `json:"temperature_c"` // nil if no sensor
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

// Collector holds per-tick state to derive network rates and the uptime store.
type Collector struct {
	uptime *UptimeStore

	prevNet     map[string]net.IOCountersStat
	prevNetTime time.Time

	// sensorErrLogged ensures a failing temperature read is logged only once,
	// not on every collection tick. collect runs in a single goroutine
	// (collectLoop), so no synchronization is needed.
	sensorErrLogged bool
}

// NewCollector builds a Collector. If uptime is nil, percentages report 100%.
func NewCollector(uptime *UptimeStore) *Collector {
	return &Collector{
		uptime:  uptime,
		prevNet: map[string]net.IOCountersStat{},
	}
}

func (col *Collector) collect(cfg Config) Snapshot {
	snap := Snapshot{Timestamp: time.Now()}

	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		snap.CPU.UsagePercent = pct[0]
	} else if err != nil {
		debugf("cpu.Percent error: %v", err)
	}

	// gopsutil may return partial results alongside a non-nil error, so use
	// whatever temps came back regardless of err. If err is non-nil and no
	// usable reading was found, log it once: this distinguishes a genuine
	// "no sensor" (temps empty, err nil) from a permission/query failure
	// (e.g. on Windows the ACPI thermal WMI class requires Administrator and
	// otherwise returns access-denied).
	temps, tempErr := sensors.SensorsTemperatures()
	if debugMode() {
		debugf("sensors: %d reading(s), err=%v", len(temps), tempErr)
		for _, t := range temps {
			debugf("    sensor %q = %.2fC", t.SensorKey, t.Temperature)
		}
	}
	for _, t := range temps {
		if cfg.TempSensorMatch == "" || strings.Contains(strings.ToLower(t.SensorKey), strings.ToLower(cfg.TempSensorMatch)) {
			v := t.Temperature
			snap.CPU.TemperatureC = &v
			break
		}
	}
	if snap.CPU.TemperatureC == nil && tempErr != nil && !col.sensorErrLogged {
		col.sensorErrLogged = true
		log.Printf("temperature unavailable: %v (temperature_c will be null; on Windows this sensor often requires running as Administrator)", tempErr)
	}

	if vm, err := mem.VirtualMemory(); err == nil {
		snap.Memory = MemoryInfo{
			TotalBytes:  vm.Total,
			UsedBytes:   vm.Used,
			UsedPercent: vm.UsedPercent,
			TotalHuman:  humanizeBytes(float64(vm.Total)),
			UsedHuman:   humanizeBytes(float64(vm.Used)),
		}
	} else {
		debugf("mem.VirtualMemory error: %v", err)
	}

	if parts, err := disk.Partitions(false); err == nil {
		for _, p := range parts {
			if len(cfg.Disks) > 0 && !contains(cfg.Disks, p.Mountpoint) {
				debugf("disk skipped (not in configured list): %s", p.Mountpoint)
				continue
			}
			usage, err := disk.Usage(p.Mountpoint)
			if err != nil {
				debugf("disk.Usage(%s) error: %v", p.Mountpoint, err)
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
	} else {
		debugf("disk.Partitions error: %v", err)
	}

	if counters, err := net.IOCounters(true); err == nil {
		now := time.Now()
		elapsed := now.Sub(col.prevNetTime).Seconds()
		if col.prevNetTime.IsZero() || elapsed <= 0 {
			elapsed = float64(cfg.IntervalSeconds)
		}
		for _, c := range counters {
			if len(cfg.Networks) > 0 {
				if !contains(cfg.Networks, c.Name) {
					debugf("nic skipped (not in configured list): %s", c.Name)
					continue
				}
			} else if isLikelyVirtual(c.Name) {
				debugf("nic skipped (looks virtual): %s", c.Name)
				continue
			}

			var rxRate, txRate float64
			if prev, ok := col.prevNet[c.Name]; ok {
				// Guard against counter resets (interface restart/reboot).
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
				m := max
				info.MaxMbps = &m
			}
			snap.Network = append(snap.Network, info)
		}
		for _, c := range counters {
			col.prevNet[c.Name] = c
		}
		col.prevNetTime = now
	} else {
		debugf("net.IOCounters error: %v", err)
	}

	if secs, err := host.Uptime(); err == nil {
		snap.Uptime.Seconds = secs
		snap.Uptime.Human = humanizeDuration(secs)
	} else {
		debugf("host.Uptime error: %v", err)
	}

	if col.uptime != nil {
		snap.Uptime.Percent7d = roundPct(col.uptime.percent(7))
		snap.Uptime.Percent14d = roundPct(col.uptime.percent(14))
		snap.Uptime.Percent30d = roundPct(col.uptime.percent(30))
		snap.Uptime.Percent365d = roundPct(col.uptime.percent(365))
	} else {
		snap.Uptime.Percent7d = 100
		snap.Uptime.Percent14d = 100
		snap.Uptime.Percent30d = 100
		snap.Uptime.Percent365d = 100
	}

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
