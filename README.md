# statusserver

A tiny Go agent that watches a machine's health and serves it to your status
page. Every few seconds it samples **CPU, load, memory, disks and network**,
and serves the result as JSON (`/api/status`), as a live websocket stream
(`/ws`), and as Prometheus metrics (`/metrics`).

- Single static binary, or a multi-arch image (`ghcr.io/lovinoes/statusserver`)
  for `linux/amd64`, `arm64`, `arm/v7` and `riscv64`.
- Zero-config start; configure with a JSON file and/or env vars.
- Token auth, CORS allow-list, per-IP websocket limits.
- Optional native TLS (TLS 1.3 only, post-quantum key exchange, HTTP/2) with
  automatic certificate reload.
- Threshold alerts to Discord, Slack or any webhook.
- Health/readiness probes and graceful shutdown.

## Documentation

| | |
| --- | --- |
| **[Installation](docs/INSTALLATION.md)** | Docker, Docker Compose, monitoring the host from Docker, and a hardened standalone install with systemd |
| **[Configuration](docs/CONFIGURATION.md)** | every option, its env var and default, alerts, TLS |
| **[API](docs/API.md)** | endpoints, auth, the websocket protocol, the response format, Prometheus metrics |

## Quick start

```bash
TOKEN=$(openssl rand -hex 32); echo "Your token: $TOKEN"

docker run -d --name statusserver --restart unless-stopped \
  -p 8090:8090 \
  -e AUTH_TOKEN="$TOKEN" \
  -e ALLOWED_ORIGINS=https://status.example.com \
  -v statusserver-data:/data \
  ghcr.io/lovinoes/statusserver:latest

curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8090/api/status
```

That container reports its own view. To monitor a server properly, run the
agent as a systemd service or use `docker-compose.host.yml`; both are covered
in the [installation guide](docs/INSTALLATION.md).

## What it reports

| | |
| --- | --- |
| **Host** | hostname, OS, platform, kernel, architecture |
| **CPU** | usage %, model, core count, load average (not on Windows), temperature (if a sensor exists) |
| **Memory** | total / used bytes and percent |
| **Storage** | every disk/mount, each against its own size |
| **Network** | per-interface throughput and totals |
| **Uptime** | host uptime, plus how reliably the agent itself has run over 7/14/30/365 days |

## Endpoints

| Path | Auth | Purpose |
| --- | --- | --- |
| `/api/status` | token | latest snapshot as JSON |
| `/ws` | token | websocket stream of snapshots |
| `/metrics` | token | Prometheus metrics |
| `/healthz` | none | liveness (`200 ok`, no system data) |
| `/readyz` | none | readiness (`200` once sampling is running) |
| `/version` | none | build info |

Send the token as `?token=...` or `Authorization: Bearer ...`.

## Using it from a website

Live updates (recommended):

```js
const ws = new WebSocket("wss://monitor.example.com/ws?token=YOUR_TOKEN");
ws.onmessage = (ev) => {
  const data = JSON.parse(ev.data);
  console.log(data.cpu.usage_percent, data.memory.used_human);
};
```

Polling:

```js
const res = await fetch("https://monitor.example.com/api/status", {
  headers: { Authorization: "Bearer YOUR_TOKEN" },
});
const data = await res.json();
```

A token used in browser code is visible to anyone who opens the page. It
keeps random scanners out, not your visitors. If the stats shouldn't be
public, put the page behind a login.

**Good to know**

- Each disk's `total_bytes` is its own size, so there's nothing to configure.
  A device mounted in several places is reported once.
- `max_mbps` only appears on an interface if you set `network_max_mbps`; use
  it to draw a "% of link" bar.
- `uptime.percent_*` measures the agent's own reliability (was it running and
  reporting?), not the host's. A fresh install starts at 100%.

## Development

```bash
cd source
go vet ./...
go test -race ./...
```

CI runs formatting, vet, race-enabled tests on the two supported Go
releases, govulncheck, and cross-builds. The Docker image is only published
after CI passes.
