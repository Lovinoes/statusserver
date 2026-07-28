# statusserver

A tiny Go agent you drop onto any machine to watch its health. It samples
**CPU, memory, every disk, and per-interface network** every few seconds and
serves it as JSON over HTTP polling *and* a websocket push stream.

Point your status website at it - directly or through nginx - and you're done.

Ships as a single static binary and a multi-arch container image
(`ghcr.io/lovinoes/statusserver`) for `linux/amd64`, `arm64`, `arm/v7`, and
`riscv64`.

## Why you might like it

- **Live push** over websocket (`/ws`) *and* plain polling (`/api/status`).
- **Zero-config start** - runs on sane defaults; override with a JSON file or
  env vars (env wins).
- **Prometheus** `/metrics` endpoint, no extra dependencies.
- **Native TLS** - serve `https`/`wss` yourself, no reverse proxy needed.
  Strict TLS 1.3-only with post-quantum key exchange (`X25519MLKEM768`,
  falling back to `X25519`/`secp384r1`) and HTTP/2.
- **Threshold alerts** to Discord / Slack / any webhook.
- **Hardened websockets** - keepalive pings reap dead clients, optional
  per-IP connection cap.
- **Graceful shutdown** plus `/healthz` and `/readyz` probes.

## What it reports

| | |
| --- | --- |
| **CPU** | temperature (where a sensor exists) + usage % |
| **Memory** | total / used bytes + percent |
| **Storage** | every disk/mount, each against its *own* size |
| **Network** | per-interface in/out throughput (raw + human string) |
| **Uptime** | seconds since boot + a human string like `27d 1h 49m 9s` |

## Quick start

Docker (configured entirely via env vars - no file needed):

```
docker run -d --name statusserver \
  -p 8090:8090 \
  -e AUTH_TOKEN=change-me \
  -e ALLOWED_ORIGINS=https://status.example.com \
  -v statusserver-data:/data \
  ghcr.io/lovinoes/statusserver:latest
```

Or from source:

```
go build -o statusserver .
./statusserver -config config.json
```

By default the container reports its *own* namespaced view. To see the real
host, mount `/proc` + `/sys` and set `HOST_PROC` / `HOST_SYS` (see the
commented block in `docker-compose.yml`).

## Configure

Three layers, each overriding the last: **defaults -> JSON file -> env vars.**
Every env var maps 1:1 to a JSON key, upper-cased (`listen_addr` ->
`LISTEN_ADDR`). Copy `config.example.json` and edit, or skip the file and use
env vars (ideal for containers).

```json
{
  "listen_addr": ":8090",
  "interval_seconds": 5,
  "auth_token": "change-me-to-a-long-random-secret",
  "allowed_origins": ["https://status.example.com"],
  "disks": ["/", "/mnt/data"],
  "networks": ["eth0"],
  "network_max_mbps": { "eth0": 1000 },
  "alerts": { "webhook_url": "", "cpu_percent": 90, "memory_percent": 90 }
}
```

Full reference (every option, its env var, default, and behaviour):
**[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)**.

## Endpoints

| Path | Auth | Purpose |
| --- | --- | --- |
| `/api/status` | token | latest snapshot as JSON |
| `/ws` | token | websocket snapshot stream |
| `/metrics` | token | Prometheus text exposition |
| `/healthz` | none | liveness (`200 ok`, no system data - safe to expose) |
| `/readyz` | none | readiness (`200` after first sample) |
| `/version` | none | build info as JSON |

Full API reference - auth, schemas, the websocket protocol, `/metrics`
output: **[`docs/API.md`](docs/API.md)**.

## Use it from your website

Live push (recommended):

```js
const ws = new WebSocket("wss://monitor.example.com/ws?token=YOUR_TOKEN");
ws.onmessage = (ev) => {
  const data = JSON.parse(ev.data);
  console.log(data.cpu.usage_percent, data.memory.used_human);
};
```

Plain polling:

```js
const res = await fetch("https://monitor.example.com/api/status", {
  headers: { Authorization: "Bearer YOUR_TOKEN" },
});
const data = await res.json();
```

## Deploy behind nginx

See `nginx.conf.example` - the key part is forwarding the `Upgrade`/
`Connection` headers on `/ws`. For a systemd host, edit and install
`statusserver.service`, then `sudo systemctl enable --now statusserver`.

## Good to know

- Each storage entry's `total_bytes` *is* its max (a 2 TB drive reports 2 TB,
  a 32 GB rootfs reports 32 GB) - no global cap to configure.
- Network has no inherent max, so `max_mbps` only appears if you set it in
  `network_max_mbps`; use it client-side to draw a "% of link" bar.
- `uptime.percent_*` reflects how reliably *this agent* has been running and
  reporting, not the host's raw uptime. A fresh install starts at 100%.

See [`docs/API.md`](docs/API.md) for the complete response shape.
