# statusserver

A tiny Go agent you install on the machine you want to monitor. It samples
CPU temperature/usage, memory, every disk/mount, and per-interface network
throughput every 5 seconds (configurable), and serves the result as JSON
over both a plain HTTP polling endpoint and a websocket push endpoint.

Your separate status website (running anywhere) connects to it - directly,
or through nginx as a reverse proxy.

## What it monitors

- **CPU**: temperature (where the OS exposes a sensor) and overall usage %.
- **Memory**: total/used bytes + percent.
- **Storage**: every local disk/partition (or just the mounts you list),
  each with its own total/used bytes and percent - so a 2 TB HDD and a
  32 GB rootfs each report against their own size, not some global max.
- **Network**: per-interface in/out throughput in bytes/sec, both as raw
  numbers and as a pre-formatted string that automatically scales between
  KiB/s, MiB/s, GiB/s, TiB/s depending on how much traffic is flowing.

## Build

```
go mod tidy     # resolves exact dependency versions + go.sum
go build -o statusserver .
```

Cross-compile for a Linux server from anywhere, e.g. from a Mac:

```
GOOS=linux GOARCH=amd64 go build -o statusserver .
```

## Configure

Copy `config.example.json` to `config.json` and edit it:

```json
{
  "listen_addr": ":8090",
  "interval_seconds": 5,
  "auth_token": "change-me-to-a-long-random-secret",
  "allowed_origins": ["https://status.example.com"],
  "disks": ["/", "/mnt/data"],
  "networks": ["eth0"],
  "network_max_mbps": { "eth0": 1000 },
  "temp_sensor_match": "coretemp"
}
```

- `disks` / `networks` empty -> auto-detects everything (real partitions,
  non-virtual interfaces). Fill them in to monitor only specific
  mounts/NICs, which is the normal case when you have several disks or
  a docker bridge you don't care about.
- `auth_token` is optional but you should set it once this is reachable
  from the internet - anyone with the URL can otherwise read your server's
  live stats. The website passes it as `?token=...` (plain WebSocket
  connections from a browser can't set custom headers) or as an
  `Authorization: Bearer ...` header for the HTTP endpoint.
- `allowed_origins` is the CORS / websocket-origin allowlist. Set it to
  your actual status website's origin instead of `"*"` once it's live.
- `temp_sensor_match` is a case-insensitive substring matched against the
  sensor name gopsutil finds (e.g. `coretemp`, `k10temp`, `cpu_thermal`).
  Leave empty to just take the first sensor found. On many VPS/cloud hosts
  there is no exposed temperature sensor at all - `temperature_c` will be
  `null` in that case, which is expected, not a bug.

## Run

```
./statusserver -config config.json
```

or install `statusserver.service` (edit the paths/user first) and:

```
sudo systemctl enable --now statusserver
```

## Reverse proxy (nginx)

See `nginx.conf.example`. The important bit is forwarding the
`Upgrade`/`Connection` headers on the `/ws` location so nginx doesn't just
treat it as a normal HTTP request.

## Consuming it from your status website

Live push (recommended - updates arrive the moment they're produced):

```js
const ws = new WebSocket("wss://monitor.example.com/ws?token=YOUR_TOKEN");
ws.onmessage = (ev) => {
  const data = JSON.parse(ev.data);
  console.log(data.cpu.usage_percent, data.memory.used_human);
};
```

Plain polling, if you'd rather pull on a timer yourself:

```js
const res = await fetch("https://monitor.example.com/api/status", {
  headers: { Authorization: "Bearer YOUR_TOKEN" },
});
const data = await res.json();
```

## Response shape

```json
{
  "timestamp": "2026-06-21T12:00:00Z",
  "cpu": { "temperature_c": 52.0, "usage_percent": 13.4 },
  "memory": {
    "total_bytes": 34359738368,
    "used_bytes": 5583457280,
    "used_percent": 16.2,
    "total_human": "32.00 GiB",
    "used_human": "5.20 GiB"
  },
  "storage": [
    {
      "mount": "/",
      "device": "/dev/sda1",
      "fstype": "ext4",
      "total_bytes": 134217728000,
      "used_bytes": 53687091200,
      "used_percent": 40.0,
      "total_human": "125.00 GiB",
      "used_human": "50.00 GiB"
    }
  ],
  "network": [
    {
      "interface": "eth0",
      "rx_bytes_per_sec": 44040192,
      "tx_bytes_per_sec": 1310720,
      "rx_human": "42.00 MiB/s",
      "tx_human": "1.25 MiB/s",
      "rx_total_bytes": 998765432100,
      "tx_total_bytes": 123456789000,
      "max_mbps": 1000
    }
  ]
}
```

Each storage entry's own `total_bytes` *is* its max (a 2 TB drive reports
2 TB, a 32 GB rootfs reports 32 GB) - no global cap to configure. Network
doesn't have an inherent max, which is why `max_mbps` is only present if
you set it in `network_max_mbps`; use it client-side to draw a "% of link
capacity" bar if you want one.
