# statusserver configuration reference

Config resolves in three layers, each overriding the last:

1. **built-in defaults** - enough to run with zero config.
2. **JSON config file** - optional, via `-config <path>` (default `config.json`
   in the working directory).
3. **environment variables** - override individual settings; env always wins.

So you can run the container with no files and set everything via env, drop in
a `config.json`, or mix both (file for the bulk, env for the secrets). Where
the file lives for each kind of install is covered in
[INSTALLATION.md](INSTALLATION.md). With the systemd setup it's
`/etc/statusserver/config.json`. Config changes take effect after a restart.

The agent checks the result at startup and refuses to start, with a clear
message, on anything that can't work: malformed JSON, a value of the wrong
type, only one of `tls_cert`/`tls_key`, an unreadable key pair, a malformed
`allowed_origins` pattern, a `webhook_url` that isn't an `http(s)://` URL, or a
port that's already taken. Recoverable mistakes fall back to the default,
with a `WARNING` in the log: unknown keys in the file (usually typos), env values
that don't parse, out-of-range numbers.

A missing config file is fine when you didn't ask for one, but if you pass
`-config` explicitly the file has to exist.

## Environment variable mapping

Every env var maps 1:1 to a JSON key, upper-cased (`listen_addr` ->
`LISTEN_ADDR`, nested `alerts.cpu_percent` -> `ALERT_CPU_PERCENT`). Only vars
that are actually **set** take effect, so env overrides the file selectively.
No prefix - bare names.

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
| `temp_sensor_match` | `TEMP_SENSOR_MATCH` | string | `""` (auto) | Sensor name substring |
| `uptime_file` | `UPTIME_FILE` | string | `uptime.json` | Uptime-history path |
| `tls_cert` | `TLS_CERT` | string | `""` | PEM certificate path |
| `tls_key` | `TLS_KEY` | string | `""` | PEM private-key path |
| `max_conns_per_ip` | `MAX_CONNS_PER_IP` | integer | `0` (unlimited) | Per-IP WS cap |
| `trust_proxy_headers` | `TRUST_PROXY_HEADERS` | boolean | `false` | Trust `X-Forwarded-For` for client IP |
| `debug` | `DEBUG` | boolean | `false` | Verbose debug logging (secrets redacted) |
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
  must be positive numbers; malformed pairs are skipped with a warning.

  ```
  NETWORK_MAX_MBPS=eth0=1000,eth1=500
  ```

- **Booleans** accept `true`/`false`, `True`/`False`, `TRUE`/`FALSE`, `1`/`0`, `t`/`f`.

## Option details

### `listen_addr`
The `host:port` the HTTP(S) server binds to. `:8090` listens on all
interfaces. Use `127.0.0.1:8090` to bind loopback-only when behind a local
reverse proxy. Empty falls back to `:8090`. The port is bound before anything
else starts, so a port conflict fails immediately.

### `interval_seconds`
How often a snapshot is collected and pushed to websocket clients. Values
`<= 0` fall back to `5`.

### `auth_token`
Shared secret required on `/api/status`, `/ws` and `/metrics`. When empty,
those endpoints are open (and the agent warns at startup unless it only
listens on loopback). Set a long random value once the agent is reachable
from the internet - anyone with the URL can otherwise read your live stats.
Clients pass it as `?token=...` (browsers/websockets) or as an
`Authorization: Bearer ...` header. See [API.md](API.md#authentication).

### `allowed_origins`
CORS and websocket-origin allow-list. Set it to your real status site's origin
instead of `"*"` once live. Empty is normalized back to `["*"]`.

Each entry is a case-insensitive glob (`*` matches anything except `/`):

- An entry containing `://` is matched against the full origin, scheme
  included: `https://status.example.com`, `https://*.example.com`.
- Any other entry is matched against the origin's host (and port) only:
  `status.example.com`, `*.example.com`, `localhost:3000`.
- `*` allows every origin.

A page served from the same host as the agent is always allowed to open the
websocket.

### `disks`
Mountpoints to report. Empty auto-detects all local partitions, and reports a
device that's mounted in several places (bind mounts - e.g. `/etc/hosts`
inside a container) only once, under its shortest mountpoint. Fill it in to
watch exactly the mounts you list. Each disk reports its own size - there is
no global cap to configure.

### `networks`
Interface names to report. Empty auto-detects, skipping loopback and common
virtual devices (docker, veth, bridges, tun/tap, CNI/flannel/calico/vxlan,
libvirt, ...). Fill it in to pick exactly the interfaces you want.

### `network_max_mbps`
Optional per-interface link capacity in Mbps. When set, the matching interface
gets a `max_mbps` field in its snapshot so the client can draw a "% of link
capacity" bar. Interfaces not listed simply omit the field.

### `temp_sensor_match`
Case-insensitive substring matched against the sensor names gopsutil finds
(e.g. `coretemp`, `k10temp`, `cpu_thermal`). Set `debug` to see every sensor
and its name in the log.

Empty (the default) picks automatically: known CPU sensors first (Intel
package temperature, AMD Tctl/Tdie, `cpu_thermal` on a Raspberry Pi, ...),
then any sensor that isn't obviously a drive, GPU or wifi card. Readings of
0 or below, or 150C and above, are ignored as bogus. Many VPS/cloud hosts
expose no sensor - `temperature_c` is `null` there, which is expected.

On **Windows** temperature comes from the ACPI thermal zone via WMI, which
usually needs the process to run **as Administrator**; without elevation the
query is denied and `temperature_c` stays `null` even if a sensor exists. The
value is a coarse ACPI zone reading, not a true per-core temp. A failed read
is logged once at startup (every attempt in `debug` mode) so you can tell "no
sensor" from "access denied".

### `uptime_file`
Where the agent persists its own uptime history for the `percent_*` figures.
Must be writable by the service. Defaults to `uptime.json` in the working
directory, which is `/var/lib/statusserver` under the bundled systemd unit;
the container image sets `UPTIME_FILE=/data/uptime.json` (the data volume).
Missing parent directories are created.

The file is written at most once a minute (to spare SD cards and SSDs) and on
clean shutdown, always atomically. A hard crash loses at most the last
minute. If the file is corrupt, it's moved aside to `uptime.json.corrupt` and
the history starts over.

### `tls_cert` / `tls_key`
Set **both** to serve HTTPS/WSS directly (no reverse proxy needed). Leave
**both** empty for plain HTTP behind a proxy. Setting only one is fatal - the
process exits at startup. The key pair is validated at startup, so a bad path
fails immediately with a clear error.

Renewed certificates are picked up without a restart: the files are checked
for changes every 30 seconds. If the new pair can't be loaded (e.g. the
certificate was written but the key not yet), the agent logs it and keeps
serving the previous one until a valid pair appears.

When TLS is on, the profile is deliberately strict:

- **TLS 1.3 only** - 1.2 and older are rejected.
- **Key exchange** (preference order): `X25519MLKEM768` (post-quantum hybrid),
  then `X25519`, then `secp384r1`. Post-quantum is preferred automatically.
- **ALPN** advertises `h2` then `http/1.1`. Normal endpoints use HTTP/2; `/ws`
  negotiates down to HTTP/1.1 (what browsers do transparently), so WSS works
  everywhere.

### `max_conns_per_ip`
Caps concurrent websocket connections from one client IP (`0` = unlimited).
When exceeded, the connection is accepted then closed with websocket status
`1013` (Try Again Later). The client IP comes from the transport connection
unless `trust_proxy_headers` is on. Negatives clamp to `0`.

### `trust_proxy_headers`
When `true`, the client IP is taken from the **last** `X-Forwarded-For` entry,
the one your reverse proxy appended, instead of the transport address.
Earlier entries are whatever the client sent and are ignored. This assumes
exactly one trusted proxy (e.g. nginx with
`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`) in front of
the agent. Enable **only** behind such a proxy, and make sure the agent isn't
reachable directly; otherwise clients can spoof the header to dodge
`max_conns_per_ip`.

### `debug`
The agent always logs one access-log line per request (client IP, method,
path, status, size, duration); a websocket's line is written when it closes.
`/healthz` and `/readyz` are left out, since orchestrators poll them
constantly. `debug` adds, on top:

- access-log lines for `/healthz` and `/readyz`
- request headers + query string (auth token, `Cookie`, `token` param redacted)
- per-tick collection detail (cpu/mem/disk/net, sensors, skipped devices,
  how long collection took)
- websocket lifecycle and per-IP rejections
- alert evaluation + webhook results
- the full effective config at startup (secrets redacted)

Safe to leave off in production; it's noisy. Toggle with `DEBUG=true` or
`"debug": true`.

### `alerts`
Posts a webhook message whenever a metric crosses its threshold, and again
when it recovers (edge-triggered, so no per-tick spam). A threshold of `0`
disables that particular check. Alerts are enabled only when `webhook_url` is
set and at least one threshold is non-zero.

- Messages start with the host's name, so several agents can share one
  channel.
- Delivery happens in the background, in order. Network errors, `429` and
  `5xx` responses are retried up to 3 times with backoff (honoring
  `Retry-After`); other errors are logged and dropped. The webhook URL (which
  usually contains a secret) never appears in logs.
- `webhook_format` selects the JSON payload shape:
  - `discord` - `{"content": "...", "allowed_mentions": {"parse": []}}` (so a
    message can never ping `@everyone`)
  - `slack` - `{"text": "..."}`
  - `generic` (default) - `{"message": "...", "timestamp": "<RFC3339>", "source": "statusserver"}`
- Thresholds are compared against the same values shown in the snapshot:
  `cpu_percent` vs `cpu.usage_percent`, `memory_percent` vs
  `memory.used_percent`, `disk_percent` vs each disk's `used_percent`, and
  `temp_c` vs `cpu.temperature_c` (skipped when there is no sensor).

## Monitoring the host from a container

These environment variables aren't agent settings. They're read by the
metrics library, and point it at the host's files when the agent runs in a
container. [`docker-compose.host.yml`](../docker-compose.host.yml) sets all of
them.

| Env var | Example | Effect |
| --- | --- | --- |
| `HOST_PROC` | `/hostfs/proc` | Read CPU, memory, uptime and the mount list from the host's `/proc` |
| `HOST_SYS` | `/hostfs/sys` | Read temperature sensors from the host's `/sys` |
| `HOST_ETC` | `/hostfs/etc` | Read the host's OS/platform info |
| `HOST_ROOT` | `/hostfs` | Measure disk usage through the host's root filesystem |

Network interfaces can't be redirected this way. The container has to use
the host's network (`network_mode: host`) to see them.

## Worked examples

- [`config.example.json`](../config.example.json) - every option, as a config file.
- [`.env.example`](../.env.example) - the common subset as env vars, for `docker-compose.yml`.
- [`docker-compose.host.yml`](../docker-compose.host.yml) - monitoring the Docker host itself.
- [`statusserver.service`](../statusserver.service) - hardened systemd unit (install steps in [INSTALLATION.md](INSTALLATION.md)).

## Command-line flags

Configuration lives in the file/env layers above; flags only control process
behaviour:

| Flag | Description |
| --- | --- |
| `-config <path>` | Path to the JSON config file (default `config.json`; must exist if given explicitly) |
| `-version` | Print build info and exit |
| `-healthcheck` | Probe local `/healthz`, exit `0`/`1` (used by the container HEALTHCHECK) |
