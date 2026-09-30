# Installing statusserver

There are two ways to run the agent:

- **[Docker](#docker)** - quickest to set up. Out of the box a container
  reports its own view; with `docker-compose.host.yml` it reports the host.
- **[Standalone with systemd](#standalone-linux--systemd)** - the binary runs
  directly on the server as a locked-down service. This is the most accurate
  way to monitor a machine.

Once it's running, see [CONFIGURATION.md](CONFIGURATION.md) for every
option and [API.md](API.md) for the endpoints.

---

## Docker

### Quick run

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

### Docker Compose

```bash
git clone https://github.com/lovinoes/statusserver.git
cd statusserver
cp .env.example .env     # then set AUTH_TOKEN, ALLOWED_ORIGINS, ...
docker compose up -d
```

`docker-compose.yml` documents every setting as a commented env var.

### Monitoring the host from Docker

A plain container only sees itself: its own network interface and its own
filesystems. To report the **host** it runs on, use `docker-compose.host.yml`
(Linux only). It uses host networking and the host's PID namespace, and
mounts the host filesystem read-only:

```bash
docker compose -f docker-compose.host.yml up -d
```

With host networking there's no port mapping. The agent listens directly on
the host at `LISTEN_ADDR` (default `:8090`), so firewall that port like any
other host service. See
[CONFIGURATION.md](CONFIGURATION.md#monitoring-the-host-from-a-container) for
what the `HOST_*` variables in that file do.

### Updating

```bash
docker compose pull && docker compose up -d
```

(Add `-f docker-compose.host.yml` to both commands if that's the file you
use.) Uptime history lives in the `statusserver-data` volume and survives
updates. The container runs as an unprivileged user and has a built-in
healthcheck.

---

## Standalone (Linux + systemd)

These steps put everything in the standard places:

| Path | What | Owner / mode |
| --- | --- | --- |
| `/usr/local/bin/statusserver` | binary | `root:root 0755` |
| `/etc/statusserver/config.json` | config (contains the token) | `root:statusserver 0640` |
| `/var/lib/statusserver/` | uptime history | created by systemd, `statusserver 0700` |

The service runs as its own unprivileged user. It can't log in or gain
privileges, it sees the filesystem read-only, and it can write only to
`/var/lib/statusserver`.

### 1. Get the binary

**Build it** (needs Go 1.26+, on any machine):

```bash
git clone https://github.com/lovinoes/statusserver.git
cd statusserver/source
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o statusserver .
```

To build for another machine, set its OS and architecture, e.g.
`GOOS=linux GOARCH=arm64` (Raspberry Pi 4/5) or `GOOS=linux GOARCH=arm GOARM=7`.

**Or copy it out of the Docker image** (no Go needed):

```bash
docker create --name statusserver-extract ghcr.io/lovinoes/statusserver:latest
docker cp statusserver-extract:/statusserver ./statusserver
docker rm statusserver-extract
```

Add `--platform linux/arm64` (or similar) to `docker create` for another
architecture.

### 2. Create the service user

A system account with no home directory and no login shell:

```bash
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin statusserver
```

### 3. Install the binary

```bash
sudo install -o root -g root -m 0755 statusserver /usr/local/bin/statusserver
statusserver -version
```

### 4. Write the config

Create the file with its final permissions *before* the token goes in, so
it's never world-readable:

```bash
sudo install -d -o root -g statusserver -m 0750 /etc/statusserver
sudo install -o root -g statusserver -m 0640 /dev/null /etc/statusserver/config.json

TOKEN=$(openssl rand -hex 32)
sudo tee /etc/statusserver/config.json > /dev/null <<EOF
{
  "listen_addr": ":8090",
  "auth_token": "$TOKEN",
  "allowed_origins": ["https://status.example.com"]
}
EOF
echo "Your token: $TOKEN"
```

Replace `https://status.example.com` with your status page's origin. Keep
the token; your status page needs it. Everything else auto-detects. For more
options (alerts, specific disks and interfaces, TLS, ...) see
[CONFIGURATION.md](CONFIGURATION.md) and
[`config.example.json`](../config.example.json). Edit later with
`sudoedit /etc/statusserver/config.json`, then restart the service.

If a reverse proxy on the same machine is the only thing that should reach
the agent, use `"listen_addr": "127.0.0.1:8090"`.

### 5. Install and start the systemd service

[`statusserver.service`](../statusserver.service) is in the repository root:

```bash
sudo install -o root -g root -m 0644 statusserver.service /etc/systemd/system/statusserver.service
sudo systemctl daemon-reload
sudo systemctl enable --now statusserver
```

Check that it's running:

```bash
systemctl status statusserver
journalctl -u statusserver -f
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8090/api/status
```

`systemd-analyze security statusserver` shows the sandboxing that's applied.

### 6. Open the firewall (if clients connect directly)

Skip this step if the agent sits behind a reverse proxy on the same host.
Otherwise allow the port, ideally only from where it's needed:

```bash
sudo ufw allow 8090/tcp                                   # anyone
sudo ufw allow from 203.0.113.10 to any port 8090 proto tcp   # one host only
```

(With firewalld: `sudo firewall-cmd --permanent --add-port=8090/tcp && sudo firewall-cmd --reload`.)

### 7. HTTPS (optional)

**Behind nginx:** see [`nginx.conf.example`](../nginx.conf.example). Set
`"listen_addr": "127.0.0.1:8090"` and `"trust_proxy_headers": true`.

**Native TLS with Let's Encrypt:** certbot's private keys are readable by
root only, so copy them to a place the service user can read, and do it again
on every renewal with a deploy hook. Save this as
`/etc/letsencrypt/renewal-hooks/deploy/statusserver.sh` and make it
executable (`sudo chmod 0755 ...`):

```sh
#!/bin/sh
set -e
dir=/etc/statusserver/tls
install -d -o root -g statusserver -m 0750 "$dir"
install -o root -g statusserver -m 0640 "$RENEWED_LINEAGE/fullchain.pem" "$dir/fullchain.pem"
install -o root -g statusserver -m 0640 "$RENEWED_LINEAGE/privkey.pem" "$dir/privkey.pem"
```

Run it once by hand for the current certificate:

```bash
sudo env RENEWED_LINEAGE=/etc/letsencrypt/live/monitor.example.com /etc/letsencrypt/renewal-hooks/deploy/statusserver.sh
```

Then add `"tls_cert": "/etc/statusserver/tls/fullchain.pem"` and
`"tls_key": "/etc/statusserver/tls/privkey.pem"` to the config and restart.
Renewed certificates are picked up automatically, no restart needed. To serve
on port 443, see the capability note in `statusserver.service`.

### Updating

```bash
sudo install -o root -g root -m 0755 statusserver /usr/local/bin/statusserver
sudo systemctl restart statusserver
```

Also reinstall `statusserver.service` (and run `systemctl daemon-reload`) if
it changed in the new version.

### Uninstalling

```bash
sudo systemctl disable --now statusserver
sudo rm /etc/systemd/system/statusserver.service /usr/local/bin/statusserver
sudo rm -r /etc/statusserver /var/lib/statusserver
sudo systemctl daemon-reload
sudo userdel statusserver
```
