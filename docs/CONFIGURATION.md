# statusserver configuration reference

`statusserver` resolves its configuration in three layers, each overriding the
previous:

1. **Built-in defaults** - enough to start with zero configuration.
2. **JSON config file** - optional, selected with `-config <path>`.
3. **Environment variables** - override individual settings; env always wins.

This layering is what makes the container workflow ergonomic: run the image
with no files on disk and configure everything with a handful of environment
variables, or drop a `config.json` in for a traditional deployment, or mix
both (file for the bulk, env for the secrets).

## Environment variable mapping

Every env var maps 1:1 to a JSON key, upper-cased. `listen_addr` becomes
`LISTEN_ADDR`, the nested `alerts.cpu_percent` becomes `ALERT_CPU_PERCENT`.
Only variables that are actually **set** take effect, so env can override the
file selectively without clobbering unset fields.

There is no prefix - the variables are bare names.

## Options

| JSON key | Env var | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `listen_addr` | `LISTEN_ADDR` | string | `:8090` | Address/port to listen on |
| `interval_seconds` | `INTERVAL_SECONDS` | integer | `5` | Sample & push interval |
| `auth_token` | `AUTH_TOKEN` | string | `""` | Bearer token; empty = open access |
| `allowed_origins` | `ALLOWED_ORIGINS` | list | `["*"]` | CORS / WS origin allow-list |
| `disks` | `DISKS` | list | `[]` (all) | Mountpoints to watch |
| `networks` | `NETWORKS` | list | `[]` (all) | Interfaces to watch |
| `network_max_mbps` | `NETWORK_MAX_MBPS` | map | `{}` | Per-NIC link capacity |
| `temp_sensor_match` | `TEMP_SENSOR_MATCH` | string | `""` | Sensor name substring |
| `uptime_file` | `UPTIME_FILE` | string | `uptime.json` | Uptime-history path |
| `tls_cert` | `TLS_CERT` | string | `""` | PEM certificate path |
| `tls_key` | `TLS_KEY` | string | `""` | PEM private-key path |
| `max_conns_per_ip` | `MAX_CONNS_PER_IP` | integer | `0` (unlimited) | Per-IP WS cap |
| `trust_proxy_headers` | `TRUST_PROXY_HEADERS` | boolean | `false` | Trust `X-Forwarded-For` for client IP |
| `alerts.webhook_url` | `ALERT_WEBHOOK_URL` | string | `""` | Webhook URL; enables alerts |
| `alerts.webhook_format` | `ALERT_WEBHOOK_FORMAT` | string | `generic` | `discord`/`slack`/`generic` |
| `alerts.cpu_percent` | `ALERT_CPU_PERCENT` | number | `0` (off) | CPU % threshold |
| `alerts.memory_percent` | `ALERT_MEMORY_PERCENT` | number | `0` (off) | Memory % threshold |
| `alerts.disk_percent` | `ALERT_DISK_PERCENT` | number | `0` (off) | Disk % threshold |
| `alerts.temp_c` | `ALERT_TEMP_C` | number | `0` (off) | Temperature threshold (C) |

### List and map formats in env vars

- **Lists** (`ALLOWED_ORIGINS`, `DISKS`, `NETWORKS`) are comma-separated.
  Whitespace around items is trimmed; empty items are dropped. An empty value
  means "unset / auto".

  ```
  ALLOWED_ORIGINS=https://status.example.com,https://admin.example.com
  DISKS=/,/mnt/data
  ```

- **Maps** (`NETWORK_MAX_MBPS`) use `key=value` pairs, comma-separated. Values
  parse as numbers; malformed pairs are skipped.

  ```
  NETWORK_MAX_MBPS=eth0=1000,eth1=500
  ```

## Option details

### `listen_addr`
The `host:port` the HTTP(S) server binds to. `:8090` listens on all
interfaces. Use `127.0.0.1:8090` to bind loopback-only when behind a local
reverse proxy. Empty falls back to `:8090`.

### `interval_seconds`
How often a snapshot is collected and pushed to websocket clients. Values
`<= 0` are clamped to `5`.

### `auth_token`
Shared secret required on `/api/status`, `/ws` and `/metrics`. When empty,
those endpoints are open. Set a long random value once the agent is reachable
from the internet - anyone with the URL can otherwise read your live stats.
Clients pass it as `?token=...` (browsers/websockets) or as an
`Authorization: Bearer ...` header. See [API.md](API.md#authentication).

### `allowed_origins`
CORS and websocket-origin allow-list. Set it to your real status site's origin
instead of `"*"` once live. Empty is normalized back to `["*"]`.

### `disks`
Mountpoints to report. Empty auto-detects all local partitions. Fill it in to
watch only specific mounts and skip docker/overlay/loop noise. Each disk
reports its own size - there is no global cap to configure.

### `networks`
Interface names to report. Empty auto-detects non-virtual interfaces. Fill it
in to skip bridges/veth you don't care about.

### `network_max_mbps`
Optional per-interface link capacity in Mbps. When set, the matching interface
gets a `max_mbps` field in its snapshot so the client can draw a "% of link
capacity" bar. Interfaces not listed simply omit the field.

### `temp_sensor_match`
Case-insensitive substring matched against the sensor name gopsutil finds
(e.g. `coretemp`, `k10temp`, `cpu_thermal`). Empty takes the first sensor
found. Many VPS/cloud hosts expose no sensor at all - `temperature_c` is
`null` there, which is expected, not a bug.

### `uptime_file`
Where the agent persists its own uptime history, used to compute the
`percent_*` reliability figures. It must live somewhere the service can write.
With the bundled systemd unit that means inside `/opt/statusserver`; in the
container the `/data` volume is the natural place. Defaults to `uptime.json`
in the working directory. Empty is normalized back to `uptime.json`.

### `tls_cert` / `tls_key`
Set **both** to serve HTTPS/WSS directly, making an external reverse proxy
optional. Leave **both** empty to serve plain HTTP behind a proxy. Setting
only one is a fatal misconfiguration - the process exits at startup.

### `max_conns_per_ip`
Caps concurrent websocket connections from a single client IP (`0` =
unlimited). Guards against a client leaking connections and exhausting
resources. When exceeded, the connection is accepted then immediately closed
with websocket status `1013` (Try Again Later). The client IP comes from the
transport connection unless `trust_proxy_headers` is enabled. Negative values
are clamped to `0`.

### `trust_proxy_headers`
When `true`, the client IP is taken from the first `X-Forwarded-For` hop
instead of the transport remote address. Enable this **only** when the agent
sits behind a trusted reverse proxy that sets the header. If left `false`
(default) while directly reachable, clients cannot spoof `X-Forwarded-For` to
evade `max_conns_per_ip`.

### `alerts`
Posts a webhook message whenever a metric crosses its threshold, and again
when it recovers (edge-triggered, so no per-tick spam). A threshold of `0`
disables that particular check. Alerts are enabled only when `webhook_url` is
set.

- `webhook_format` selects the JSON payload shape:
  - `discord` - `{"content": "..."}`
  - `slack` - `{"text": "..."}`
  - `generic` (default) - `{"message": "...", "timestamp": "<RFC3339>", "source": "statusserver"}`
- Thresholds are compared against the same values shown in the snapshot:
  `cpu_percent` vs `cpu.usage_percent`, `memory_percent` vs
  `memory.used_percent`, `disk_percent` vs each disk's `used_percent`, and
  `temp_c` vs `cpu.temperature_c` (skipped when there is no sensor).

## Example config file

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
  "trust_proxy_headers": false,
  "tls_cert": "",
  "tls_key": "",
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

## Equivalent environment-only configuration

```
LISTEN_ADDR=:8090
INTERVAL_SECONDS=5
AUTH_TOKEN=change-me-to-a-long-random-secret
ALLOWED_ORIGINS=https://status.example.com
DISKS=/,/mnt/data
NETWORKS=eth0
NETWORK_MAX_MBPS=eth0=1000
TEMP_SENSOR_MATCH=coretemp
UPTIME_FILE=/data/uptime.json
MAX_CONNS_PER_IP=10
TRUST_PROXY_HEADERS=false
ALERT_WEBHOOK_URL=https://discord.com/api/webhooks/...
ALERT_WEBHOOK_FORMAT=discord
ALERT_CPU_PERCENT=90
ALERT_MEMORY_PERCENT=90
ALERT_DISK_PERCENT=90
ALERT_TEMP_C=85
```

## Command-line flags

Configuration lives in the file/env layers above; flags only control process
behaviour:

| Flag | Description |
| --- | --- |
| `-config <path>` | Path to the JSON config file (default `config.json`) |
| `-version` | Print build info and exit |
| `-healthcheck` | Probe local `/healthz`, exit `0`/`1` (used by the container HEALTHCHECK) |
