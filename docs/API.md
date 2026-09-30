# statusserver API reference

`statusserver` exposes a small HTTP API plus a websocket stream. All
system-data endpoints share the same simple bearer-token auth; the probes and
build-info endpoints are always open.

- Base URL: `http(s)://<host>:8090` (port and scheme depend on your config).
- Content type: JSON unless noted otherwise.
- All timestamps are RFC 3339 / ISO 8601 in UTC.
- Every `GET` endpoint also answers `HEAD`. Any other method gets
  `405 Method Not Allowed` with an `Allow` header.

When native TLS is enabled the server enforces TLS 1.3 only with post-quantum
key exchange (`X25519MLKEM768`, then `X25519`/`secp384r1`) and advertises HTTP/2
via ALPN. Regular endpoints are served over HTTP/2; the `/ws` WebSocket uses
HTTP/1.1 (negotiated automatically via ALPN), which browsers handle
transparently.

## Authentication

Auth applies only when `auth_token` (env `AUTH_TOKEN`) is set. If it is empty,
every endpoint is open.

When a token is configured, `/api/status`, `/ws` and `/metrics` require it,
supplied in **either** of two ways:

| Method | Example | When to use |
| --- | --- | --- |
| Query parameter | `...?token=YOUR_TOKEN` | browsers / websockets (can't set headers) |
| `Authorization` header | `Authorization: Bearer YOUR_TOKEN` | server-to-server HTTP |

The `Bearer` scheme is case-insensitive. The comparison is constant-time. A
missing or wrong token returns `401 Unauthorized` with a
`WWW-Authenticate: Bearer` header.

## Endpoint summary

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `GET` | `/api/status` | token | Latest snapshot as JSON |
| `GET` | `/ws` | token | Websocket snapshot stream |
| `GET` | `/metrics` | token | Prometheus text exposition |
| `GET` | `/healthz` | none | Liveness probe |
| `GET` | `/readyz` | none | Readiness probe |
| `GET` | `/version` | none | Build info |

`OPTIONS` is handled on `/api/status` for CORS preflight and returns
`204 No Content`.

---

## `GET /api/status`

Returns the most recently collected [snapshot](#snapshot-object).

- `200 OK` - body is a snapshot object.
- `401 Unauthorized` - token required/incorrect.
- `503 Service Unavailable` - no snapshot has been collected yet (only in the
  first moments after startup). Body: `{"error": "no snapshot collected yet"}`,
  with a `Retry-After: 1` header.

Responses carry `Cache-Control: no-store`.

CORS: if the request `Origin` matches `allowed_origins` (see
[CONFIGURATION.md](CONFIGURATION.md#allowed_origins) for the matching rules),
the response echoes it in `Access-Control-Allow-Origin` and allows the
`Authorization` header.

```bash
curl -H "Authorization: Bearer YOUR_TOKEN" \
  https://monitor.example.com/api/status
```

---

## `GET /ws`

Upgrades to a websocket and streams snapshots.

Protocol:

1. On connect, the server sends the current snapshot straight away (or the
   first one as soon as it's collected, right after startup).
2. After that it sends a fresh snapshot every `interval_seconds`.
3. Each message is a single text frame containing one JSON snapshot object.
4. A client that can't keep up skips intermediate snapshots and always gets
   the newest one; it never holds up other clients.
5. The server pings every 30s; dead connections are dropped automatically.
6. The client is not expected to send anything. Sending a data message closes
   the connection with status `1008` (Policy Violation).
7. On shutdown the server closes connections with status `1001` (Going Away).

The websocket `Origin` is validated against `allowed_origins`; a same-host
origin is always accepted. A disallowed origin gets `403 Forbidden` before
the upgrade. If `max_conns_per_ip` is set and exceeded, the server accepts
then immediately closes with status `1013` (Try Again Later).

```js
const ws = new WebSocket("wss://monitor.example.com/ws?token=YOUR_TOKEN");
ws.onmessage = (ev) => {
  const snap = JSON.parse(ev.data);
  console.log(snap.cpu.usage_percent, snap.memory.used_human);
};
```

---

## `GET /metrics`

Prometheus text exposition format (version 0.0.4). Auth-gated like
`/api/status`. All series are prefixed `statusserver_`.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `statusserver_build_info` | gauge | `version`, `commit`, `go` | Always `1`; build metadata |
| `statusserver_ws_clients` | gauge | - | Connected websocket clients |
| `statusserver_scrape_time_seconds` | gauge | - | Unix time of the snapshot |
| `statusserver_host_info` | gauge | `hostname`,`os`,`platform`,`platform_version`,`kernel_version`,`arch` | Always `1`; host identification |
| `statusserver_cpu_usage_percent` | gauge | - | CPU utilization 0-100 |
| `statusserver_cpu_cores` | gauge | - | Logical CPU cores |
| `statusserver_cpu_temperature_celsius` | gauge | - | CPU temp (omitted if no sensor) |
| `statusserver_load_average` | gauge | `window` (`1m`/`5m`/`15m`) | Load average (omitted on Windows) |
| `statusserver_memory_total_bytes` | gauge | - | Total RAM |
| `statusserver_memory_used_bytes` | gauge | - | Used RAM |
| `statusserver_memory_used_percent` | gauge | - | Used RAM 0-100 |
| `statusserver_disk_total_bytes` | gauge | `mount`,`device`,`fstype` | Disk size |
| `statusserver_disk_used_bytes` | gauge | `mount`,`device`,`fstype` | Disk used |
| `statusserver_disk_used_percent` | gauge | `mount`,`device`,`fstype` | Disk used 0-100 |
| `statusserver_network_rx_bytes_per_second` | gauge | `interface` | RX throughput |
| `statusserver_network_tx_bytes_per_second` | gauge | `interface` | TX throughput |
| `statusserver_network_rx_bytes_total` | counter | `interface` | RX since boot |
| `statusserver_network_tx_bytes_total` | counter | `interface` | TX since boot |
| `statusserver_host_uptime_seconds` | gauge | - | Host uptime since boot |
| `statusserver_agent_uptime_percent` | gauge | `window` (`7d`/`14d`/`30d`/`365d`) | Agent reporting reliability |

Before the first snapshot is collected only `statusserver_build_info` and
`statusserver_ws_clients` are emitted, to avoid publishing misleading zeros.

Example scrape config:

```yaml
scrape_configs:
  - job_name: statusserver
    authorization:
      credentials: YOUR_TOKEN
    static_configs:
      - targets: ["monitor.example.com:8090"]
```

---

## `GET /healthz`

Unauthenticated liveness probe. Always returns `200 OK` with the body `ok`
while the process is running. Carries no system data, so it is safe to expose
publicly. Used by the container `HEALTHCHECK` (via `statusserver -healthcheck`).

## `GET /readyz`

Unauthenticated readiness probe.

- `200 OK` (`ready`) once a snapshot has been collected and sampling is
  keeping up.
- `503 Service Unavailable` before the first snapshot (`not ready`), or when
  the latest snapshot is older than `3 x interval_seconds + 30s` (`stale: ...`),
  which means collection is stuck (e.g. on a hung network filesystem).

## `GET /version`

Unauthenticated build info as JSON:

```json
{
  "version": "1.2.3",
  "commit": "abc1234",
  "date": "2026-01-01T00:00:00Z",
  "go": "go1.27.1",
  "os": "linux",
  "arch": "amd64"
}
```

---

## Snapshot object

The payload returned by `/api/status` and pushed over `/ws`:

```json
{
  "timestamp": "2026-06-21T12:00:00Z",
  "host": {
    "hostname": "web-1",
    "os": "linux",
    "platform": "debian",
    "platform_version": "13.1",
    "kernel_version": "6.12.48-1-amd64",
    "arch": "x86_64"
  },
  "cpu": {
    "temperature_c": 52.0,
    "usage_percent": 13.4,
    "model": "AMD Ryzen 7 5700G with Radeon Graphics",
    "cores": 16,
    "load": { "load1": 0.42, "load5": 0.35, "load15": 0.3 }
  },
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
    "human": "27d 11h 29m 9s",
    "percent_7d": 100,
    "percent_14d": 99.8,
    "percent_30d": 99.95,
    "percent_365d": 99.9
  }
}
```

### Fields

**`timestamp`** (string) - when the snapshot was collected (RFC 3339, UTC).

**`host`** (object) - static host identification, read once at startup.

| Field | Type | Notes |
| --- | --- | --- |
| `hostname` | string | |
| `os` | string | e.g. `linux`, `windows`, `darwin` |
| `platform` | string | Distribution/product, e.g. `debian`, `Microsoft Windows 11 Pro` |
| `platform_version` | string | e.g. `13.1` |
| `kernel_version` | string | |
| `arch` | string | Machine architecture, e.g. `x86_64`, `aarch64` |

Any of these can be an empty string when the OS doesn't expose it.

**`cpu`** (object)

| Field | Type | Notes |
| --- | --- | --- |
| `temperature_c` | number \| null | `null` when no sensor is exposed (common on VPS) |
| `usage_percent` | number | Overall CPU utilization since the previous sample, 0-100 |
| `model` | string | CPU model name (may be empty) |
| `cores` | integer | Logical cores |
| `load` | object \| null | `load1`/`load5`/`load15` load averages; `null` on Windows, which has none |

**`memory`** (object)

| Field | Type | Notes |
| --- | --- | --- |
| `total_bytes` | integer | Total physical memory |
| `used_bytes` | integer | Used physical memory |
| `used_percent` | number | 0-100 |
| `total_human` | string | e.g. `32.00 GiB` |
| `used_human` | string | e.g. `5.20 GiB` |

**`storage`** (array of objects, never `null`) - one entry per watched mount.

| Field | Type | Notes |
| --- | --- | --- |
| `mount` | string | Mountpoint, e.g. `/` (`C:` on Windows) |
| `device` | string | Backing device, e.g. `/dev/sda1` |
| `fstype` | string | Filesystem type, e.g. `ext4` |
| `total_bytes` | integer | Size of this filesystem |
| `used_bytes` | integer | Used space |
| `used_percent` | number | 0-100 |
| `total_human` | string | e.g. `125.00 GiB` |
| `used_human` | string | e.g. `50.00 GiB` |

Each entry's `total_bytes` is its own max - a 2 TB drive reports 2 TB, a
32 GB rootfs reports 32 GB. There is no global cap to configure.

**`network`** (array of objects, never `null`) - one entry per watched
interface.

| Field | Type | Notes |
| --- | --- | --- |
| `interface` | string | Interface name, e.g. `eth0` |
| `rx_bytes_per_sec` | number | Receive rate (0 in the very first snapshot) |
| `tx_bytes_per_sec` | number | Transmit rate (0 in the very first snapshot) |
| `rx_human` | string | Auto-scaled, e.g. `42.00 MiB/s` |
| `tx_human` | string | Auto-scaled, e.g. `1.25 MiB/s` |
| `rx_total_bytes` | integer | Cumulative received since boot |
| `tx_total_bytes` | integer | Cumulative transmitted since boot |
| `max_mbps` | number | Present only if set via `network_max_mbps` |

`max_mbps` is omitted unless you configure it; use it client-side to draw a
"% of link capacity" bar.

**`uptime`** (object)

| Field | Type | Notes |
| --- | --- | --- |
| `seconds` | integer | Host uptime, seconds since boot |
| `human` | string | e.g. `27d 11h 29m 9s` |
| `percent_7d` | number | Agent reporting reliability, trailing 7 days |
| `percent_14d` | number | ...trailing 14 days |
| `percent_30d` | number | ...trailing 30 days |
| `percent_365d` | number | ...trailing 365 days |

`percent_*` reflects how reliably *this agent* has been running and
reporting, not the host's raw uptime. A host up for 27 days straight but whose
statusserver service crash-looped for an hour last week shows a high `seconds`
but `percent_7d` a little under 100. Time before the agent was first started
doesn't count against it, so a brand new install starts at 100%. Days are UTC
days.

## Status codes

| Code | Meaning |
| --- | --- |
| `200 OK` | Success |
| `204 No Content` | CORS preflight (`OPTIONS /api/status`) |
| `401 Unauthorized` | Missing/incorrect token |
| `403 Forbidden` | Websocket `Origin` not in `allowed_origins` |
| `404 Not Found` | Unknown path |
| `405 Method Not Allowed` | Wrong HTTP method |
| `503 Service Unavailable` | No snapshot yet (`/api/status`, `/readyz`) or collection stuck (`/readyz`) |
