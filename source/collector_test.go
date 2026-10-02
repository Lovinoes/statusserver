package main

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/sensors"
)

func TestIsLikelyVirtual(t *testing.T) {
	virtual := []string{"lo", "lo0", "lo:1", "Loopback Pseudo-Interface 1", "docker0", "veth1234",
		"br-abcdef", "virbr0", "vnet3", "tun0", "tap0", "cni0", "flannel.1", "cali123", "vxlan.calico", "kube-ipvs0",
		"fwbr100i0", "fwpr100p0", "fwln100i0", "tap100i0", "veth101i0"}
	for _, n := range virtual {
		if !isLikelyVirtual(n) {
			t.Errorf("expected %q to be treated as virtual", n)
		}
	}
	physical := []string{"eth0", "enp3s0", "wlan0", "ens18", "Ethernet", "Wi-Fi", "Local Area Connection", "wg0", "bond0", "en0", "vmbr0", "eno1"}
	for _, n := range physical {
		if isLikelyVirtual(n) {
			t.Errorf("did not expect %q to be treated as virtual", n)
		}
	}
}

func temps(pairs ...any) []sensors.TemperatureStat {
	var out []sensors.TemperatureStat
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, sensors.TemperatureStat{SensorKey: pairs[i].(string), Temperature: pairs[i+1].(float64)})
	}
	return out
}

func TestPickTemperature(t *testing.T) {
	cases := []struct {
		name   string
		in     []sensors.TemperatureStat
		match  string
		want   float64
		wantOK bool
	}{
		{"none", nil, "", 0, false},
		{"prefers cpu package over nvme listed first",
			temps("nvme_composite", 38.0, "coretemp_core_0", 50.0, "coretemp_package_id_0", 55.0), "", 55, true},
		{"amd tctl", temps("amdgpu_edge", 60.0, "k10temp_tctl", 48.0), "", 48, true},
		{"raspberry pi", temps("cpu_thermal", 45.0), "", 45, true},
		{"fallback to unknown sensor", temps(`ACPI\ThermalZone\TZ00_0`, 27.8), "", 27.8, true},
		{"never a drive or gpu", temps("nvme_composite", 38.0, "amdgpu_edge", 60.0), "", 0, false},
		{"skips invalid readings", temps("coretemp_package_id_0", 0.0, "coretemp_core_0", 51.0), "", 51, true},
		{"explicit match wins", temps("coretemp_package_id_0", 55.0, "acpitz", 30.0), "ACPITZ", 30, true},
		{"explicit match with no hit", temps("coretemp_package_id_0", 55.0), "k10temp", 0, false},
	}
	for _, c := range cases {
		got, ok := pickTemperature(c.in, c.match)
		if ok != c.wantOK || got != c.want {
			t.Errorf("%s: got %v,%v want %v,%v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSelectPartitions(t *testing.T) {
	parts := []disk.PartitionStat{
		{Device: "overlay", Mountpoint: "/"},
		{Device: "/dev/sda1", Mountpoint: "/etc/resolv.conf"},
		{Device: "/dev/sda1", Mountpoint: "/etc/hosts"},
		{Device: "/dev/sda1", Mountpoint: "/data"},
		{Device: "/dev/sdb1", Mountpoint: "/mnt/b"},
		{Device: "", Mountpoint: "/weird"},
	}
	mounts := func(ps []disk.PartitionStat) string {
		var m []string
		for _, p := range ps {
			m = append(m, p.Mountpoint)
		}
		return strings.Join(m, ",")
	}

	if got := mounts(selectPartitions(parts, nil)); got != "/,/data,/mnt/b,/weird" {
		t.Errorf("auto = %s", got)
	}
	// an explicit list is honored exactly, duplicates and all.
	if got := mounts(selectPartitions(parts, []string{"/etc/hosts", "/data"})); got != "/etc/hosts,/data" {
		t.Errorf("explicit = %s", got)
	}
}

// TestCollectSmoke runs a real collection against this machine.
func TestCollectSmoke(t *testing.T) {
	cfg := defaultConfig()
	col := NewCollector(cfg, newMemStore())
	ctx := context.Background()
	col.collect(ctx)
	time.Sleep(50 * time.Millisecond)
	snap := col.collect(ctx)

	if snap.Timestamp.IsZero() || snap.Timestamp.Location() != time.UTC {
		t.Errorf("timestamp = %v, want non-zero UTC", snap.Timestamp)
	}
	if snap.Memory.TotalBytes == 0 {
		t.Error("memory total should be known")
	}
	if snap.CPU.Cores <= 0 || snap.Host.OS == "" || snap.Host.Arch == "" {
		t.Errorf("cpu/host info missing: %+v %+v", snap.CPU, snap.Host)
	}
	if snap.CPU.UsagePercent < 0 || snap.CPU.UsagePercent > 100 {
		t.Errorf("cpu usage out of range: %v", snap.CPU.UsagePercent)
	}
	for _, d := range snap.Storage {
		if d.TotalBytes == 0 || d.Mount == "" {
			t.Errorf("bad storage entry %+v", d)
		}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"storage":[`, `"network":[`, `"host":{`, `"load":`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("json missing %s: %s", key, data)
		}
	}
}

func TestHostPath(t *testing.T) {
	t.Setenv("HOST_ROOT", "")
	if got := hostPath("/data"); got != "/data" {
		t.Errorf("no HOST_ROOT: %q", got)
	}
	t.Setenv("HOST_ROOT", "/")
	if got := hostPath("/data"); got != "/data" {
		t.Errorf("HOST_ROOT=/: %q", got)
	}
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("HOST_ROOT", "/hostfs")
	for in, want := range map[string]string{"/": "/hostfs", "/data": "/hostfs/data", "/mnt/b/": "/hostfs/mnt/b"} {
		if got := hostPath(in); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", in, got, want)
		}
	}
}
