package main

import (
	"cmp"
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
)

type Snapshot struct {
	Timestamp time.Time     `json:"timestamp"`
	Host      HostInfo      `json:"host"`
	CPU       CPUInfo       `json:"cpu"`
	Memory    MemoryInfo    `json:"memory"`
	Storage   []StorageInfo `json:"storage"`
	Network   []NetworkInfo `json:"network"`
	Uptime    UptimeInfo    `json:"uptime"`
}

// HostInfo is static host identification, gathered once at startup.
type HostInfo struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelVersion   string `json:"kernel_version"`
	Arch            string `json:"arch"`
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
	Model        string   `json:"model"`
	Cores        int      `json:"cores"` // logical cores
	Load         *LoadAvg `json:"load"`  // nil where the OS has no load average (windows)
}

type LoadAvg struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
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

// Collector gathers snapshots. It is not safe for concurrent use; only the
// collection loop calls it.
type Collector struct {
	cfg    Config
	uptime *UptimeStore

	host     HostInfo
	cpuModel string
	cpuCores int

	prevNet     map[string]net.IOCountersStat
	prevNetTime time.Time

	// sensorErrLogged makes a failing temperature read log only once.
	sensorErrLogged bool
}

func NewCollector(cfg Config, uptime *UptimeStore) *Collector {
	col := &Collector{
		cfg:     cfg,
		uptime:  uptime,
		prevNet: map[string]net.IOCountersStat{},
	}

	col.host.Arch = runtime.GOARCH
	col.host.OS = runtime.GOOS
	if hi, err := host.Info(); err == nil {
		col.host = HostInfo{
			Hostname:        hi.Hostname,
			OS:              cmp.Or(hi.OS, runtime.GOOS),
			Platform:        hi.Platform,
			PlatformVersion: hi.PlatformVersion,
			KernelVersion:   hi.KernelVersion,
			Arch:            cmp.Or(hi.KernelArch, runtime.GOARCH),
		}
	} else {
		debugf("host.Info error: %v", err)
	}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		col.cpuModel = strings.TrimSpace(infos[0].ModelName)
	} else if err != nil {
		debugf("cpu.Info error: %v", err)
	}
	if n, err := cpu.Counts(true); err == nil {
		col.cpuCores = n
	} else {
		col.cpuCores = runtime.NumCPU()
	}
	return col
}

func (col *Collector) collect(ctx context.Context) Snapshot {
	cfg := col.cfg
	snap := Snapshot{
		Timestamp: time.Now().UTC(),
		Host:      col.host,
		CPU:       CPUInfo{Model: col.cpuModel, Cores: col.cpuCores},
		// empty rather than nil so clients always get a json array.
		Storage: []StorageInfo{},
		Network: []NetworkInfo{},
	}

	// interval 0 compares against the previous call, i.e. usage since the
	// last tick.
	if pct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pct) > 0 {
		snap.CPU.UsagePercent = pct[0]
	} else if err != nil {
		debugf("cpu.Percent error: %v", err)
	}

	// windows has no load average; gopsutil only emulates one there.
	if runtime.GOOS != "windows" {
		if avg, err := load.AvgWithContext(ctx); err == nil {
			snap.CPU.Load = &LoadAvg{Load1: avg.Load1, Load5: avg.Load5, Load15: avg.Load15}
		} else {
			debugf("load.Avg error: %v", err)
		}
	}

	// gopsutil may hand back partial results alongside a non-nil error, so use
	// whatever temps came back. log a genuine failure once (on windows the
	// thermal wmi class often needs administrator).
	temps, tempErr := sensors.TemperaturesWithContext(ctx)
	if debugMode() {
		debugf("sensors: %d reading(s), err=%v", len(temps), tempErr)
		for _, t := range temps {
			debugf("    sensor %q = %.2fC", t.SensorKey, t.Temperature)
		}
	}
	if v, ok := pickTemperature(temps, cfg.TempSensorMatch); ok {
		snap.CPU.TemperatureC = &v
	} else if tempErr != nil && !col.sensorErrLogged {
		col.sensorErrLogged = true
		log.Printf("temperature unavailable: %v (temperature_c will be null; on Windows this sensor often requires running as Administrator)", tempErr)
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
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

	if parts, err := disk.PartitionsWithContext(ctx, false); err == nil {
		for _, p := range selectPartitions(parts, cfg.Disks) {
			usage, err := disk.UsageWithContext(ctx, hostPath(p.Mountpoint))
			if err != nil {
				debugf("disk.Usage(%s) error: %v", p.Mountpoint, err)
				continue
			}
			if usage.Total == 0 {
				debugf("disk skipped (zero size): %s", p.Mountpoint)
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

	if counters, err := net.IOCountersWithContext(ctx, true); err == nil {
		now := time.Now()
		elapsed := now.Sub(col.prevNetTime).Seconds()
		first := col.prevNetTime.IsZero()
		for _, c := range counters {
			if len(cfg.Networks) > 0 {
				if !slices.Contains(cfg.Networks, c.Name) {
					debugf("nic skipped (not in configured list): %s", c.Name)
					continue
				}
			} else if isLikelyVirtual(c.Name) {
				debugf("nic skipped (looks virtual): %s", c.Name)
				continue
			}

			var rxRate, txRate float64
			if prev, ok := col.prevNet[c.Name]; ok && !first && elapsed > 0 {
				// guard against counter resets (interface restart/reboot).
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
			if m, ok := cfg.NetworkMaxMbps[c.Name]; ok && m > 0 {
				info.MaxMbps = &m
			}
			snap.Network = append(snap.Network, info)
		}
		// rebuild so interfaces that disappear don't linger forever.
		col.prevNet = make(map[string]net.IOCountersStat, len(counters))
		for _, c := range counters {
			col.prevNet[c.Name] = c
		}
		col.prevNetTime = now
	} else {
		debugf("net.IOCounters error: %v", err)
	}

	if secs, err := host.UptimeWithContext(ctx); err == nil {
		snap.Uptime.Seconds = secs
		snap.Uptime.Human = humanizeDuration(secs)
	} else {
		debugf("host.Uptime error: %v", err)
	}

	now := time.Now()
	snap.Uptime.Percent7d = roundPct(col.uptime.Percent(7, now))
	snap.Uptime.Percent14d = roundPct(col.uptime.Percent(14, now))
	snap.Uptime.Percent30d = roundPct(col.uptime.Percent(30, now))
	snap.Uptime.Percent365d = roundPct(col.uptime.Percent(365, now))

	return snap
}

// hostPath maps a host mountpoint to where it's visible to us. In a container
// monitoring its host, the host's root filesystem is bind-mounted somewhere
// (HOST_ROOT, e.g. /hostfs) and HOST_PROC makes gopsutil list the host's
// mounts, but it would still measure their size at the container's own path.
func hostPath(mount string) string {
	root := os.Getenv("HOST_ROOT")
	if root == "" || root == "/" || runtime.GOOS == "windows" {
		return mount
	}
	return filepath.Join(root, mount)
}

// selectPartitions picks the partitions to report. With an explicit list,
// exactly those mounts are kept. Otherwise every partition is kept, but a
// device mounted in several places (bind mounts, e.g. /etc/hosts inside a
// container) is reported once, under its shortest mountpoint.
func selectPartitions(parts []disk.PartitionStat, wanted []string) []disk.PartitionStat {
	if len(wanted) > 0 {
		var out []disk.PartitionStat
		for _, p := range parts {
			if slices.Contains(wanted, p.Mountpoint) {
				out = append(out, p)
			} else {
				debugf("disk skipped (not in configured list): %s", p.Mountpoint)
			}
		}
		return out
	}

	best := map[string]int{} // device -> index into parts
	for i, p := range parts {
		if p.Device == "" {
			continue
		}
		j, seen := best[p.Device]
		if !seen || len(p.Mountpoint) < len(parts[j].Mountpoint) {
			best[p.Device] = i
		}
	}
	var out []disk.PartitionStat
	for i, p := range parts {
		if j, ok := best[p.Device]; ok && j != i {
			debugf("disk skipped (device %s already reported via %s): %s", p.Device, parts[j].Mountpoint, p.Mountpoint)
			continue
		}
		out = append(out, p)
	}
	return out
}

// cpuSensorHints are sensor key fragments that identify a CPU reading, most
// specific first (package / die temperature beats a single core).
var cpuSensorHints = []string{
	"coretemp_package", "k10temp_tctl", "k10temp_tdie", "zenpower_tdie", "zenpower_tctl",
	"coretemp", "k10temp", "zenpower", "cpu_thermal", "cpu", "soc_thermal", "acpitz",
}

// nonCPUSensorHints are sensors that are never the CPU (drives, GPUs, wifi).
var nonCPUSensorHints = []string{"nvme", "drivetemp", "amdgpu", "radeon", "nouveau", "iwlwifi", "ath1", "mt79", "gpu"}

// pickTemperature chooses the CPU temperature from the sensor list. With a
// match string, the first sensor containing it wins. Otherwise known CPU
// sensors are preferred, falling back to any sensor that isn't obviously a
// drive/GPU/radio. Implausible readings (<= 0 or >= 150C) are ignored.
func pickTemperature(temps []sensors.TemperatureStat, match string) (float64, bool) {
	valid := func(t sensors.TemperatureStat) bool {
		return t.Temperature > 0 && t.Temperature < 150
	}
	find := func(pred func(key string) bool) (float64, bool) {
		for _, t := range temps {
			if valid(t) && pred(strings.ToLower(t.SensorKey)) {
				return t.Temperature, true
			}
		}
		return 0, false
	}

	if match != "" {
		m := strings.ToLower(match)
		return find(func(k string) bool { return strings.Contains(k, m) })
	}
	for _, hint := range cpuSensorHints {
		if v, ok := find(func(k string) bool { return strings.Contains(k, hint) }); ok {
			return v, true
		}
	}
	return find(func(k string) bool {
		for _, h := range nonCPUSensorHints {
			if strings.Contains(k, h) {
				return false
			}
		}
		return true
	})
}

// virtualNICPrefixes are interface name prefixes for loopback, container,
// bridge, tunnel and overlay devices that aren't worth reporting by default.
var virtualNICPrefixes = []string{
	"loopback", "docker", "veth", "br-", "virbr", "vnet", "tun", "tap", "cni", "flannel",
	"cali", "vxlan", "kube-", "lxcbr", "lxdbr", "podman", "dummy", "ifb",
	// proxmox per-guest firewall plumbing (fwbr100i0, fwpr100p0, fwln100i0).
	"fwbr", "fwpr", "fwln",
}

func isLikelyVirtual(name string) bool {
	n := strings.ToLower(name)
	// loopback is "lo", "lo0" (bsd/macos) or an alias like "lo:1", but a bare
	// "lo" prefix would also catch windows' "Local Area Connection".
	if rest, ok := strings.CutPrefix(n, "lo"); ok {
		if rest == "" || rest[0] == ':' || strings.Trim(rest, "0123456789") == "" {
			return true
		}
	}
	for _, p := range virtualNICPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
