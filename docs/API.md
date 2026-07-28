# statusserver API reference

`statusserver` exposes a small HTTP API plus a websocket stream. All
system-data endpoints share the same simple bearer-token auth; the probes and
build-info endpoints are always open.

- Base URL: `http(s)://<host>:8090` (port and scheme depend on your config).
- Content type: JSON unless noted otherwise.
- All timestamps are RFC 3339 / ISO 8601 in UTC.

## Authentication

Auth applies only when `auth_token` (env `AUTH_TOKEN`) is set. If it is empty,
every endpoint is open.

When a token is configured, `/api/status`, `/ws` and `/metrics` require it,
supplied in **either** of two ways:

| Method | Example | When to use |
| --- | --- | --- |
| Query parameter | `...?token=YOUR_TOKEN` | browsers / websockets (can't set headers) |
| `Authorization` header | `Authorization: Bearer YOUR_TOKEN` | server-to-server HTTP |

The comparison is constant-time. A missing or wrong token returns
`401 Unauthorized`.

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
- `405 Method Not Allowed` - method other than `GET`/`OPTIONS`.

CORS: if the request `Origin` is in `allowed_origins`, the response echoes
`Access-Control-Allow-Origin` and related headers.

```bash
curl -H "Authorization: Bearer YOUR_TOKEN" \
  https://monitor.example.com/api/status
```

---

## `GET /ws`

Upgrades to a websocket and streams snapshots.

Protocol:

1. On connect, the server immediately sends the current snapshot.
2. Thereafter it sends a fresh snapshot every `interval_seconds`.
3. Each message is a single text frame containing one JSON snapshot object.
4. The server pings every 30s; dead connections are dropped automatically.
5. The client is not expected to send anything. Incoming frames are read and
   discarded (control frames are handled).

The websocket origin is validated against `allowed_origins`. If
`max_conns_per_ip` is set and exceeded, the server accepts then immediately
closes with status `1013` (Try Again Later).

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
| `statusserver_cpu_usage_percent` | gauge | - | CPU utilization 0-100 |
| `statusserver_cpu_temperature_celsius` | gauge | - | CPU temp (omitted if no sensor) |
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

- `200 OK` (`ready`) once the first snapshot has been collected.
- `503 Service Unavailable` (`not ready`) before that.

## `GET /version`

Unauthenticated build info as JSON:

```json
{
  "version": "1.2.3",
  "commit": "abc1234",
  "date": "2026-01-01T00:00:00Z",
  "go": "go1.24.0",
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

### Fields

**`timestamp`** (string) - when the snapshot was collected (RFC 3339, UTC).

**`cpu`** (object)

| Field | Type | Notes |
| --- | --- | --- |
| `temperature_c` | number \| null | `null` when no sensor is exposed (common on VPS) |
| `usage_percent` | number | Overall CPU utilization, 0-100 |

**`memory`** (object)

| Field | Type | Notes |
| --- | --- | --- |
| `total_bytes` | integer | Total physical memory |
| `used_bytes` | integer | Used physical memory |
| `used_percent` | number | 0-100 |
| `total_human` | string | e.g. `32.00 GiB` |
| `used_human` | string | e.g. `5.20 GiB` |

**`storage`** (array of objects) - one entry per watched mount.

| Field | Type | Notes |
| --- | --- | --- |
| `mount` | string | Mountpoint, e.g. `/` |
| `device` | string | Backing device, e.g. `/dev/sda1` |
| `fstype` | string | Filesystem type, e.g. `ext4` |
| `total_bytes` | integer | Size of this filesystem |
| `used_bytes` | integer | Used space |
| `used_percent` | number | 0-100 |
| `total_human` | string | e.g. `125.00 GiB` |
| `used_human` | string | e.g. `50.00 GiB` |

Each entry's `total_bytes` is its own max - a 2 TB drive reports 2 TB, a
32 GB rootfs reports 32 GB. There is no global cap to configure.

**`network`** (array of objects) - one entry per watched interface.

| Field | Type | Notes |
| --- | --- | --- |
| `interface` | string | Interface name, e.g. `eth0` |
| `rx_bytes_per_sec` | number | Receive rate |
| `tx_bytes_per_sec` | number | Transmit rate |
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
| `human` | string | e.g. `27d 11h 22m 29s` |
| `percent_7d` | number | Agent reporting reliability, trailing 7 days |
| `percent_14d` | number | ...trailing 14 days |
| `percent_30d` | number | ...trailing 30 days |
| `percent_365d` | number | ...trailing 365 days |

`percent_*` reflects how reliably *this agent* has been running and
reporting, not the host's raw uptime. A host up for 27 days straight but whose
statusserver service crash-looped for an hour last week shows a high `seconds`
but `percent_7d` a little under 100. A brand new install starts at 100%
(there is no history yet to penalize it).

## Status codes

| Code | Meaning |
| --- | --- |
| `200 OK` | Success |
| `204 No Content` | CORS preflight (`OPTIONS /api/status`) |
| `401 Unauthorized` | Missing/incorrect token |
| `405 Method Not Allowed` | Wrong HTTP method |
| `503 Service Unavailable` | Not ready yet (`/readyz`) |
