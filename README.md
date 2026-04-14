# usenet-pipeline

A self-hosted agent that automates the torrent-to-Usenet pipeline: downloads torrents, extracts media metadata, generates PAR2 recovery, optionally encrypts, uploads to Usenet via NNTP, and reports back to a companion indexer site with a complete NZB.

Designed to run on a VPS behind a VPN with zero manual intervention.

## Features

- Torrent downloading via magnet links (anacrolix/torrent, pure Go)
- Usenet uploading with parallel NNTP connections and yEnc encoding
- PAR2 recovery block generation (parpar or par2cmdline)
- Optional 7z encryption with header obfuscation
- Optional filename obfuscation (random hex subjects)
- Media analysis via ffprobe (codec, resolution, HDR detection)
- Automatic screenshot capture via ffmpeg
- VPN integration via gluetun (full-tunnel or split-tunnel)
- Disk space reservation and CPU throttling
- Slow/dead download detection and automatic rejection
- Live status reporting to companion site (speed, phase, VPN IP)
- Web-configurable settings (no restart needed for most changes)

## Quick Start

### 1. Prerequisites

- Docker + Docker Compose
- A running indexer site instance with an agent token
- Usenet provider credentials
- VPN credentials (recommended)

### 2. Configure

```bash
cp .env.example .env
nano .env
```

**Required settings:**

| Variable | Description |
|----------|-------------|
| `SITE_URL` | Your indexer site URL |
| `AGENT_TOKEN` | Agent token from Account Settings |
| `NNTP_SERVER` | Usenet server address (e.g. `news.provider.com:563`) |
| `NNTP_SSL` | `true` for TLS connections |
| `NNTP_USER` / `NNTP_PASS` | Usenet credentials |
| `NNTP_GROUP` | Target newsgroup |
| `NNTP_POSTER` | Poster identity (e.g. `agent <agent@your-domain.com>`) |
| `NNTP_FROM` | From-header address for posted articles |
| `NNTP_DOMAIN` | Domain used when generating Message-IDs |

See [.env.example](.env.example) for the full list with descriptions.

### 3. Build and start

```bash
docker compose build --no-cache
docker compose up -d
```

### 4. Verify

```bash
docker compose logs -f agent
```

You should see:
```
Polling for work...
No task available, sleeping 30s
```

## VPN Modes

### Full tunnel (default)

All agent traffic routes through gluetun. Acts as an automatic kill-switch — if the VPN drops, nothing leaks.

```env
VPN_DOWNLOAD_ONLY=false
```

### Split tunnel

Only torrent downloads go through the VPN (via SOCKS5 proxy). NNTP uploads and site API calls go direct for full speed.

```env
VPN_DOWNLOAD_ONLY=true
AGENT_NETWORK_MODE=bridge
```

Gluetun exposes a SOCKS5 proxy on port 1080. The torrent client is automatically configured to use it.

## How It Works

```
Poll site for task
  |
  v
Download torrent (VPN-protected)
  |
  v
Analyze: ffprobe metadata + screenshots
  |
  v
Generate PAR2 recovery blocks
  |
  v
Optional: 7z encryption
  |
  v
Upload to Usenet via NNTP
  |
  v
Report NZB + metadata to site
  |
  v
Poll for next task
```

## Architecture

```
+----------+     +----------+     +-----------+
| Gluetun  | <-- |  Agent   | --> | NNTP      |
| (VPN)    |     |          |     | (Usenet)  |
+----------+     +----+-----+     +-----------+
                      |
                      | HTTPS
                      v
                +----------+
                | Indexer   |
                | Site      |
                +----------+
```

In full-tunnel mode, all traffic goes through gluetun. In split-tunnel mode, only torrent traffic uses the VPN's SOCKS5 proxy.

## Configuration Layers

Settings come from three tiers, merged in this order (highest wins):

1. **Web overrides** — pushed down from the site UI on each poll. Live-applied; no restart.
2. **`.env` / process environment** — connection-sensitive values (site, NNTP, VPN credentials). Restart required.
3. **`config.yml`** — on-disk tunables the agent can rewrite when you click "Save to agent config" in the UI.

`config.yml` lives next to the agent (or at `$CONFIG_YML` / `$CONFIG_DIR/config.yml`). Keys in `.env` (VPN_*, NNTP_*, `SITE_URL`, `AGENT_TOKEN`) are never written into it. The agent reports its local snapshot on every poll so the site can show which tier each value came from.

## Web Dashboard Configuration

Most settings can be changed from the site UI without restarting:

- **Mode**: Auto (picks requests) or Manual (only assigned tasks)
- **Speed limits**: Max download/upload KB/s
- **Pool**: Filter by category
- **Shutdown rules**: After N downloads, minutes, or points
- **Torrent watch folder**: Monitor a directory for completed downloads
- **Built-in FTP server**: Accept uploads directly

## Local UI

The agent also serves a small loopback-only page for editing secrets and on-disk config without the site. Enable by setting `LOCAL_UI_PORT`; bind beyond `127.0.0.1` with `LOCAL_UI_BIND` (not recommended). Auth token is stored in `secrets.yml` alongside any private-tracker passkeys — neither ever leaves the agent.

## Processing Options

| Variable | Default | Description |
|----------|---------|-------------|
| `ENCRYPT` | `false` | Wrap files in password-protected 7z |
| `OBFUSCATE` | `false` | Rename files to random hex on Usenet |
| `PAR2_REDUNDANCY` | `5` | Recovery block percentage |
| `PAR2_THREADS` | `0` | CPU threads for PAR2 (0 = all) |
| `PAR2_MEMORY_MB` | `0` | PAR2 memory cap in MB (0 = auto) |
| `MAX_CONCURRENT_DOWNLOADS` | `3` | Parallel torrent downloads |
| `MAX_DISK_USAGE_GB` | `0` | Disk cap (0 = no limit) |
| `CPU_MAX_PERCENT` | `85` | Pause new tasks above this CPU % |
| `SLOW_SPEED_THRESHOLD_MBS` | `0.05` | Reject downloads below this speed |
| `SLOW_SPEED_TIMEOUT_MINS` | `10` | Minutes before slow rejection |
| `GENERATOR_NAME` | `usenet-pipeline` | NZB `x-generator` header value |
| `VPN_PROXY_ADDR` | `vpn:1080` | Gluetun SOCKS5 proxy address (split-tunnel) |
| `DATA_DIR` | `./data` | Host dir for agent state, downloads, gluetun config |
| `LOCAL_UI_PORT` | *(unset)* | Enable local UI on this port (loopback) |
| `LOCAL_UI_BIND` | `127.0.0.1` | Address the local UI listens on |

### Tunable via `config.yml` / web UI

These live in `config.yml` (or are pushed from the site) rather than `.env`:

| Key | Default | Description |
|-----|---------|-------------|
| `torrent_max_upload_kbps` | `0` | Per-torrent upload cap (0 = unlimited) |
| `torrent_seed_ratio` | `0` | Stop seeding once this ratio is reached |
| `torrent_seed_hours` | — | Stop seeding after this many hours |
| `torrent_require_full_seed` | — | Wait for a full seed before considering complete |
| `torrent_no_full_seed_timeout_mins` | — | Abort if no full seed within this window |
| `torrent_port` | `0` | Torrent listen port (0 = random) |
| `low_peers_threshold` | — | Skip torrents with ≤ this many seeders |
| `low_peers_timeout_mins` | — | Minutes of sustained low peers before skip |
| `max_concurrent_downloads` | `3` | Parallel torrent downloads |
| `cpu_max_percent` | `85` | Pause new tasks above this CPU % |
| `max_disk_usage_gb` | `0` | Disk cap (0 = no limit) |
| `slow_speed_threshold_mbs` | `0.05` | Reject downloads below this speed |
| `slow_speed_timeout_mins` | `10` | Minutes before slow rejection |
| `local_ui_port` | *(unset)* | Local UI port |
| `local_ui_bind` | `127.0.0.1` | Local UI bind address |

## Updating

```bash
docker compose build --no-cache
docker compose up -d
```

## Troubleshooting

**Agent can't connect to site**
- Verify `SITE_URL` is reachable from the VPS
- Check `AGENT_TOKEN` matches a token in Account Settings
- If using VPN: confirm VPN is up first (`docker compose logs vpn`)

**VPN won't connect**
- Check credentials in `.env`
- Try a different country/server
- See [gluetun docs](https://github.com/qdm12/gluetun-wiki) for your provider

**Upload failures**
- Verify NNTP credentials and newsgroup
- Check connection count doesn't exceed provider limits
- Look for retry errors in logs

**No tasks picked up**
- Set mode to "auto" on the dashboard
- Check pool filter isn't excluding available requests
- Verify there are unfulfilled requests on the site

## Built With

### Docker / Infrastructure

- [Gluetun](https://github.com/qdm12/gluetun) — VPN client container with kill-switch and SOCKS5 proxy
- [ParPar](https://github.com/animetosho/ParPar) — high-performance, multi-threaded PAR2 generator
- [par2cmdline](https://github.com/Parchive/par2cmdline) — PAR2 recovery (fallback when ParPar is unavailable)
- [FFmpeg](https://github.com/FFmpeg/FFmpeg) — screenshot capture and media processing
- [7-Zip](https://github.com/ip7z/7zip) — encryption with header obfuscation

### Go Libraries

- [anacrolix/torrent](https://github.com/anacrolix/torrent) — pure Go BitTorrent client
- [go-ffprobe](https://github.com/vansante/go-ffprobe) — FFprobe wrapper for media metadata extraction
- [google/uuid](https://github.com/google/uuid) — UUID generation for NNTP Message-IDs

NNTP uploading and yEnc encoding are implemented from scratch using Go's standard library (`net/textproto`, `crypto/tls`).

## License

[MIT](LICENSE)
