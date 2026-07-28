# statusserver

A tiny Go agent you install on the machine you want to monitor. It samples
CPU temperature/usage, memory, every disk/mount, and per-interface network
throughput every 5 seconds (configurable), and serves the result as JSON
over both a plain HTTP polling endpoint and a websocket push endpoint.

Your separate status website (running anywhere) connects to it - directly,
or through nginx as a reverse proxy.

It ships as a single static binary and a multi-arch container image
(`ghcr.io/lovinoes/statusserver`) for `linux/amd64`, `arm64`, `arm/v7`, and
`riscv64`.

## Features

- Live metrics over HTTP (`/api/status`) and websocket (`/ws`).
- Zero-config startup with layered configuration: defaults, then a JSON
  file, then environment variables (env wins).
- Prometheus-compatible `/metrics` endpoint (dependency-free exporter).
- Optional native TLS (serve `https`/`wss` directly, no reverse proxy
  required).
- Optional threshold alerts posted to a Discord / Slack / generic webhook.
- Hardened websockets: keepalive pings reap dead connections, and an
  optional per-IP connection cap.
- Graceful shutdown, liveness (`/healthz`) and readiness (`/readyz`) probes.

## What it monitors

- **CPU**: temperature (where the OS exposes a sensor) and overall usage %.
- **Memory**: total/used bytes + percent.
- **Storage**: every local disk/partition (or just the mounts you list),
  each with its own total/used bytes and percent - so a 2 TB HDD and a
  32 GB rootfs each report against their own size, not some global max.
- **Network**: per-interface in/out throughput in bytes/sec, both as raw
  numbers and as a pre-formatted string that automatically scales between
  KiB/s, MiB/s, GiB/s, TiB/s depending on how much traffic is flowing.
- **Uptime**: current host uptime - seconds since boot, plus a human string
  like `27d 1h 49m 9s`.

## Build

```
go mod tidy     # resolves exact dependency versions + go.sum
go build -o statusserver .
```

Cross-compile for a Linux server from anywhere, e.g. from a Mac:

```
GOOS=linux GOARCH=amd64 go build -o statusserver .
```

Stamp version info into the binary (what `-version` and `/metrics` report):

```
go build -ldflags "-s -w \
  -X main.version=1.2.3 \
  -X main.commit=$(git rev-parse --short HEAD) \
  -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o statusserver .
```

## Run with Docker

The image is published to GHCR and configured entirely via environment
variables, so no config file is needed:

```
docker run -d --name statusserver \
  -p 8090:8090 \
  -e AUTH_TOKEN=change-me \
  -e ALLOWED_ORIGINS=https://status.example.com \
  -v statusserver-data:/data \
  ghcr.io/lovinoes/statusserver:latest
```

Or with Compose (see `docker-compose.yml`; copy `.env.example` to `.env` to
set values without editing the file):

```
docker compose up -d
```

By default the container reports its **own** namespaced view of the system.
To report the real host metrics, mount `/proc` and `/sys` and set
`HOST_PROC` / `HOST_SYS` (see the commented block in `docker-compose.yml`).

## Configure

Configuration is resolved in three layers, each overriding the previous:

1. built-in defaults,
2. an optional JSON file (`-config config.json`),
3. environment variables (`AUTH_TOKEN`, `LISTEN_ADDR`, ...).

Each env var maps 1:1 to a JSON key, upper-cased (`listen_addr` ->
`LISTEN_ADDR`). Copy `config.example.json` to `config.json` and edit it, or
skip the file entirely and use environment variables (ideal for containers).

**See [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) for the full reference**
(every option, its env var, default, and behaviour). A quick example config
file:

```json
{
  "listen_addr": ":8090",
  "interval_seconds": 5,
  "auth_token": "change-me-to-a-long-random-secret",
  "allowed_origins": ["https://status.example.com"],
  "disks": ["/", "/mnt/data"],
  "networks": ["eth0"],
  "network_max_mbps": { "eth0": 1000 },
  "temp_sensor_match": "coretemp",
  "uptime_file": "uptime.json",
  "max_conns_per_ip": 10,
  "alerts": {
    "webhook_url": "",
    "webhook_format": "discord",
    "cpu_percent": 90,
    "memory_percent": 90,
    "disk_percent": 90,
    "temp_c": 85
  }
}
```

## Run

```
./statusserver -config config.json
```

Useful flags:

- `-version` prints build info and exits.
- `-healthcheck` probes the local `/healthz` and exits `0`/`1` (used by the
  container `HEALTHCHECK`).

Install `statusserver.service` (edit the paths/user first) and:

```
sudo systemctl enable --now statusserver
```

## Endpoints

| Path | Auth | Purpose |
| --- | --- | --- |
| `/api/status` | token | latest snapshot as JSON |
| `/ws` | token | websocket snapshot stream |
| `/metrics` | token | Prometheus text exposition |
| `/healthz` | none | liveness probe (`200 ok`) |
| `/readyz` | none | readiness (`200` after first sample) |
| `/version` | none | build info as JSON |

`/healthz` carries no system data, so it's safe to expose.

**See [`docs/API.md`](docs/API.md) for the full API reference** - auth,
request/response schemas, the websocket protocol, and the `/metrics` output.

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
  ],
  "uptime": {
    "seconds": 2374149,
    "human": "27d 11h 22m 29s",
    "percent_7d": 100,
    "percent_14d": 99.8,
    "percent_30d": 99.95,
    "percent_365d": 99.9
  }
}
```

`percent_*` reflects how reliably *this agent* has been running and
reporting, not the host's raw uptime - a host that's been up for 27 days
straight but had the statusserver service crash-looping for an hour last
week would show `seconds: 2374149` but `percent_7d` a little under 100.
A brand new install always starts at 100% (there's no history yet to
penalize it for).

Each storage entry's own `total_bytes` *is* its max (a 2 TB drive reports
2 TB, a 32 GB rootfs reports 32 GB) - no global cap to configure. Network
doesn't have an inherent max, which is why `max_mbps` is only present if
you set it in `network_max_mbps`; use it client-side to draw a "% of link
capacity" bar if you want one.
