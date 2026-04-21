# usenet-pipeline

A self-hosted agent that automates the torrent-to-Usenet pipeline: downloads torrents, extracts media metadata, generates PAR2 recovery, optionally encrypts, uploads to Usenet via NNTP, and produces an NZB — either reported back to a companion indexer site or saved to a local output folder for standalone/offline use.

Designed to run on a VPS behind a VPN with zero manual intervention.

## Features

**Pipeline**
- Torrent downloading via magnet links or private `.torrent` files (anacrolix/torrent, pure Go)
- Private tracker support with `secrets.yml` passkey store and DHT avoidance
- Usenet uploading with parallel NNTP connections and yEnc encoding
- PAR2 recovery block generation (parpar or par2cmdline)
- Optional 7z encryption with header obfuscation
- Optional filename obfuscation (random hex subjects)
- Media analysis via ffprobe (codec, resolution, HDR detection)
- Type-aware sampling: video screenshots, manga page extracts, music clips
- Optional screenshot watermarking
- VPN integration via gluetun (full-tunnel or split-tunnel)

**Standalone / offline uploads**
- Watch folders — drop a `.torrent`, a completed release folder, or a single media file and the agent produces an NZB locally
- Groups concept — each watch folder is tagged with a group that controls newsgroups, sampling strategy, PAR2%, obfuscation, watermark, and a per-group banned-extensions list
- Output layout per-release: `<group>/<title>/{title.nzb, screenshots/, password.txt}`
- Site-push sync — when a companion site publishes a catalog at `GET /api/agent/groups`, the agent pulls it in and upserts as `source='site'`. Locally-defined groups always win

**Local web UI** (loopback-only by default)
- Sidebar with live speed + VPN + disk widgets, two-colour speed sparkline via SSE
- Edit `config.yml` + private-tracker passkeys without SSH
- CRUD for groups, watch folders, job history
- `/events` SSE stream for building your own dashboards

**Resilience**
- Agent self-heal: if the site has been unreachable for 5 min, the agent pokes gluetun's HTTP control API to reconnect; after 10 min it exits for the supervisor to restart
- Disk space reservation and CPU throttling
- Slow/dead download detection and automatic rejection (even for torrents stuck at 99.x%)
- Versioned agent↔site protocol with upgrade gating and maintenance-mode backoff
- Failed-completion backfill: on-disk NZB snapshots auto-resubmitted on restart

## Quick Start

### 1. Prerequisites

- Docker + Docker Compose v2
- A running indexer site instance with an agent token
- Usenet provider credentials
- VPN credentials (recommended)

### 2. Install

Use the packaged release (simplest):

```bash
# Linux/macOS
./release/install.sh

# Windows
release\install.bat
```

The installer checks Docker, creates the data dirs, copies `.env.example` → `.env`, and prompts you to edit before bringing the stack up.

Or configure manually:

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

## Agent Self-Heal

The agent has a small watchdog goroutine that nudges a stuck VPN back to life before resorting to a full process restart. It tracks the last successful HTTP roundtrip to the site and escalates:

| Stalled for | Action |
|-------------|--------|
| ≥ 5 min | `PUT /v1/openvpn/status` + `/v1/wireguard/status` on `GLUETUN_CONTROL_URL` (default `http://127.0.0.1:8000` — same netns as the agent). 3-minute cooldown between attempts. |
| ≥ 10 min | `os.Exit(1)` so `restart: unless-stopped` / systemd can take over with a fresh process. |

Requires gluetun's HTTP control server on — the stock `docker-compose.yml` sets `HTTP_CONTROL_SERVER_ADDRESS=:8000` explicitly so this works out of the box.

A separate change in the same release raised the torrent stall-cancel's percent gate: a torrent stuck at 99.x % with 0 peers now gets rejected after `torrent_no_full_seed_timeout_mins`, same as one stuck earlier. Dead torrents no longer hold slots forever.

## How It Works

```
Poll site for task
  |
  v
Download torrent (VPN-protected; fetches .torrent over HTTPS for private releases)
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
Report NZB + metadata to site (gzipped multipart; auto-retry on site maintenance)
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
           +----------+----------+
           | HTTPS              local
           v              ./data/agent/offline-output
     +-----------+
     | Indexer   |
     | Site      |
     +-----------+
```

In full-tunnel mode, all traffic goes through gluetun. In split-tunnel mode, only torrent traffic uses the VPN's SOCKS5 proxy. Watch-folder jobs write their NZBs to `OFFLINE_OUTPUT_DIR` on disk instead of posting back to the site.

## Offline Uploads

Separate from the site-polling loop, the agent can watch filesystem folders and turn dropped content into NZBs on its own — useful for posting your own library, for sites that don't (yet) implement the agent protocol, or as a backup destination alongside the normal flow.

### Concepts

- **Group** — a posting category. Name, type (`video` / `manga` / `music` / any custom label), list of newsgroups, and per-group overrides: sample count, audio sample seconds, PAR2 %, obfuscation, watermark text, banned-extensions blocklist. Groups are either edited locally via the UI or pushed down from the site (`source='site'`).
- **Watch folder** — an absolute path on disk tagged with one group. Every file or subfolder that appears under it becomes a queued job.
- **Offline job** — tracks one detected input through the pipeline: `queued → processing → completed | failed`. Retry / delete actions available from the `/jobs` page.

### Flow

1. Open the local UI (`http://localhost:4869/` by default).
2. **Groups** page: create a group, pick its type, paste your target newsgroups (one per line — multiple newsgroups produce a cross-post).
3. **Watches** page: add a watch folder, point it at an absolute path, assign the group.
4. Drop a `.torrent`, a release folder, or a single media file into that path.
5. Within ~15 seconds it appears on the **Jobs** page as `queued`; the processor claims it, downloads/stages/PAR2s/uploads, and writes:

   ```
   $OFFLINE_OUTPUT_DIR/<group-name>/<title>/
     ├── <title>.nzb
     ├── screenshots/    # or samples/ (manga pages, audio clips, depending on type)
     └── password.txt    # only when ENCRYPT=true
   ```

### Type-aware sampling

Each group has a `type` that selects which preview the agent generates:

| Type | What it samples |
|------|-----------------|
| `video` | PNG screenshots via ffmpeg, evenly spaced across the middle 90% of the longest video file. Optional watermark drawn in the bottom-right. |
| `manga` | Pages extracted from the first CBZ/CBR/EPUB archive found. |
| `music` | MP3 clips (VBR ~165 kbps) of configurable duration from the longest audio track. |
| *(other)* | Sampling skipped; the NZB is still produced and posted. |

Set `type` in the Groups UI. Unknown / custom values (e.g. `audiobook`, `software`) don't cause errors — the agent just posts raw files without a preview. This keeps custom site-pushed types forward-compatible.

### Per-group blocklist

Each group carries a list of extensions to strip from the release before staging. An empty list inherits the hardcoded default (the 90-entry Microsoft "high-risk file types" set + common scripts/executables). A non-empty list *replaces* the default — useful when:

- a music group legitimately ships `.iso` alongside audio
- a video group wants to additionally block `.html` / `.url`

### Output path

`OFFLINE_OUTPUT_DIR` defaults to `$TempDir/../offline-output`. With the stock docker-compose bind-mount that resolves to `./data/agent/offline-output` on the host.

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

The agent serves a browser UI at `http://localhost:${LOCAL_UI_PORT}/` (default `4869`, published by docker-compose bound to `127.0.0.1` on the host). Five pages + an SSE stream:

| Path | What it does |
|------|--------------|
| `/` | Edit `config.yml` keys and private-tracker passkeys in `secrets.yml`. Each row shows which tier the current value came from (env / yml / default). |
| `/groups` | CRUD for posting groups — name, type, newsgroups, overrides, watermark, banned extensions. Site-pushed rows (`source='site'`) render read-only. |
| `/watches` | CRUD for watch folders. Each folder is tagged with a group. |
| `/jobs` | Offline job history — status badge, phase, created timestamp, retry/delete actions. |
| `/events` | Server-sent events. Pushes a snapshot every ~1.5 s: phase, MB/s, VPN state, disk free/reserved, job counts per status. The layout's sidebar subscribes to it for the live speed graph and the active-jobs count badge next to the Jobs nav link. |

Bind beyond `127.0.0.1` by setting `LOCAL_UI_BIND` (not recommended unless you put a reverse proxy with auth in front — nothing in `/`, `/groups`, or `/secrets` requires login yet).

Passkeys and secrets stay on the agent's disk at all times — the site is told *that* trackers are configured, never the keys.

## Private Trackers

When the site marks a request as private, the agent:

1. Fetches the `.torrent` file from the site over HTTPS (no magnet lookup).
2. Skips DHT and PEX so the info hash never leaks off the private tracker.
3. Injects the matching tracker URL with its passkey from `secrets.yml`.

Passkeys are edited via the local UI only. The site never sees them.

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
| `LOCAL_UI_PORT` | `4869` | Local UI port (published to `127.0.0.1` on the host by docker-compose) |
| `LOCAL_UI_BIND` | `127.0.0.1` (`0.0.0.0` inside docker) | Address the local UI listens on inside the container |
| `OFFLINE_OUTPUT_DIR` | `<TempDir>/../offline-output` | Where offline-job NZBs + sidecars get written |
| `GLUETUN_CONTROL_URL` | `http://127.0.0.1:8000` | Watchdog target for VPN restart. Lives in the shared netns — no exposure to the host needed. |

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

**"Agent upgrade required" / HTTP 426**
- Your site requires a newer agent protocol version. Pull the latest release and rebuild.

**Completion uploads backed up on disk**
- Look for `backup-request-*.nzb` in the data directory. These are NZBs the agent couldn't report because the site was unreachable at the time. They're re-submitted automatically on the next startup (via `/api/agent/backfill`) and deleted on success.

---

## Server API

The agent talks to its companion site over a small JSON/HTTPS protocol. This section is the full contract so you can write your own server. The reference implementation is [client/client.go](client/client.go).

### Conventions

- **Base URL**: `SITE_URL` from the agent's `.env`. All paths below are relative to this.
- **Auth**: `Authorization: Bearer <AGENT_TOKEN>` on every request. Tokens are opaque strings issued per-agent by your server.
- **Version headers** (sent on every request):
  - `X-Agent-Protocol: <int>` — current protocol is `2`.
  - `X-Agent-Version: <string>` — build version (e.g. `1.2.0`). Informational.
- **Content**: JSON unless noted. Large payloads (`/complete`, `/backfill`, `/screenshot`) are **gzipped multipart** (`Content-Encoding: gzip`, `Content-Type: multipart/form-data; boundary=...`).
- **Timeouts**: agent uses a 120s HTTP timeout per request.

### Global status codes

These apply to most endpoints and the agent reacts to them specifically:

| Code | Meaning | Agent behaviour |
|------|---------|-----------------|
| `200` | OK | Parse body. |
| `204` | No content | Treated as "no task" on `/poll`. |
| `401 Unauthorized` | Bad/expired token | Logs and backs off. |
| `403 Forbidden` | IP not approved for this token | Surfaces error; user must approve in site UI. |
| `404 Not Found` | Endpoint not implemented | On optional endpoints (`/local-config`, `/directives`) the agent treats as empty. |
| `426 Upgrade Required` | Agent protocol below site minimum | Pauses polling ~10 min. Body: `{"min_protocol": N, "message": "..."}`. |
| `503 Service Unavailable` | Site in maintenance (if body JSON has `maintenance:true`) | Agent waits `eta_seconds` + 15s and retries. |

### Maintenance response

Returned with `503` when the site wants agents to back off:

```json
{
  "maintenance": true,
  "reason": "VACUUM FULL in progress",
  "started_at": 1713312000,
  "elapsed_seconds": 45,
  "eta_seconds": 180
}
```

---

### 1. Lifecycle — startup

#### `POST /api/agent/clear-locks`

Called once at agent startup to release any locks this agent still holds from a previous crash.

- **Request body**: *(empty)*
- **Response**: `{"cleared": <int>}` — count of locks expired.

#### `POST /api/agent/backfill`

Resubmit an NZB from a local backup (used when a previous `/complete` failed). The site should dedupe by NZB hash and fulfil the referenced request as if `/complete` had succeeded.

- **Content-Encoding**: `gzip`
- **Form fields**:
  | Field | Type | Required | Notes |
  |-------|------|----------|-------|
  | `request_id` | string (int64) | yes | Original request this NZB satisfies. |
  | `password` | string | no | If the release was encrypted. |
  | `nzb_data` | file | yes | The `.nzb` file bytes. |
- **Response**: `{"nzb_id": <int64>}`
- **Idempotency**: must be safe to retry. The agent only deletes its local backup after a `200`.

---

### 2. Lifecycle — polling loop

The agent runs this loop every `PollInterval` seconds (default 30s).

#### `GET /api/agent/config`

Server-side tunables. Returned values with `0`/empty mean "use local default".

- **Response**:
  ```json
  {
    "max_concurrent": 3,
    "cpu_max_percent": 85,
    "max_disk_usage_gb": 0,
    "slow_speed_threshold": 0.05,
    "slow_speed_timeout": 10,
    "low_peers_threshold": 0,
    "low_peers_timeout": 0,
    "web_overrides": { "torrent_port": "51413" }
  }
  ```
  `web_overrides` is the authoritative web-tier map for the agent's layered config. Keys absent here fall through to env/yml.

#### `POST /api/agent/local-config`

Snapshot of the agent's on-disk + env config so the site UI can show provenance badges. Returning `404` here is allowed — the agent treats it as "site doesn't support this yet".

- **Request body**:
  ```json
  {
    "on_disk_writable": true,
    "has_private_trackers": false,
    "local_ui_url": "http://127.0.0.1:7878",
    "yml_path": "/app/data/config.yml",
    "local": {
      "torrent_port": { "value": "51413", "source": "yml" },
      "cpu_max_percent": { "value": "85", "source": "env" }
    }
  }
  ```
- **Response**: ignored.

#### `GET /api/agent/directives`

Queued instructions for this agent. The agent acks each one after processing it.

- **Response**:
  ```json
  {
    "directives": [
      { "id": 101, "kind": "write_config", "payload": { "updates": { "torrent_port": "51413" } } }
    ]
  }
  ```
  Current kinds:
  - `write_config` — payload `{"updates": {"<key>": "<value>"}}`. Agent writes to `config.yml`.

#### `POST /api/agent/directives/ack`

- **Request body**: `{"id": <int64>, "error": ""}` — `error` is empty on success, message on failure.
- **Response**: ignored.

#### `GET /api/agent/groups`

Optional. Publishes a catalog of posting groups for the agent to upsert into its local DB with `source='site'`. Returning `404` is allowed — the agent treats it as "no catalog" and keeps running with locally-defined groups only.

- **Query**: `?since_version=N` where `N` is the `max_version` the agent received on its previous poll (`0` on first boot and after a reconciliation pass).
- **Response**:
  ```json
  {
    "max_version": 42,
    "groups": [
      {
        "id": 1,
        "name": "anime",
        "type": "video",
        "newsgroups": ["alt.binaries.multimedia.anime.highspeed"],
        "banned_extensions": [".exe", ".html"],
        "screenshots": 6,
        "sample_seconds": null,
        "par2_redundancy": 5,
        "obfuscate": true,
        "watermark_text": "-YourTag",
        "version": 42
      }
    ]
  }
  ```
  - Nullable fields (`screenshots`, `sample_seconds`, `par2_redundancy`, `obfuscate`) mean "agent falls back to its own default."
  - `type` is an open string — the agent skips sampling for unknown values but still posts. Lets the server add categories (`audiobook`, `software`, ...) without an agent update.
  - `max_version` is the current top of the catalog across all rows; the agent sends it back on its next poll so steady-state fetches return an empty `groups` array when nothing's changed.
- **Delete semantics**: there's no tombstone. The agent performs a full `since_version=0` fetch on startup and deletes any local `source='site'` row whose name isn't in that list. Steady-state polls are incremental only.
- **Local override wins**: if an agent already has a locally-defined group with the same name, the site's version is logged-and-skipped — operators can delete their local row to let the site take over.

#### `POST /api/agent/poll`

Request work. Return **one** of three shapes:

Task assigned:
```json
{
  "request_id": 987654321,
  "lock_id": 42,
  "title": "Some Release Title",
  "nyaa_url": "https://nyaa.si/view/1234",
  "info_hash": "deadbeefcafebabe...",
  "category": "anime",
  "season": "2024-Winter",
  "episodes": "1-13",
  "boost_count": 0,
  "private": false,
  "torrent_file_url": "/api/agent/torrent/42"
}
```
No work available:
```json
{ "reason": "no available requests" }
```
Command to the agent:
```json
{ "command": "stop" }
```

Field notes:
- `lock_id` identifies this agent's hold on the request and is echoed in `/progress`, `/status`, and `/complete`.
- `private: true` **requires** `torrent_file_url` to be set — the agent will fetch the file instead of resolving via DHT/magnet. Only available to v2+ agents.
- Return `204 No Content` for legacy "no task" (still honoured).

#### `POST /api/agent/log`

Fire-and-forget log line for dashboard display.

- **Request body**: `{"level": "info|warn|error", "message": "..."}`
- **Response**: ignored.

---

### 3. Lifecycle — during a task

#### `GET <torrent_file_url>` (typically `/api/agent/torrent/{lock_id}`)

Returns the raw `.torrent` file bytes for private tasks. The URL is whatever path you returned in `AgentTask.torrent_file_url`.

- **Response**: `application/x-bittorrent` body, ≤ 10 MB.
- **404**: lock expired or not private — agent aborts the task.

#### `POST /api/agent/progress`

Short, human-readable progress string. Throttled to ~10s by the agent.

- **Request body**: `{"lock_id": 42, "progress": "Downloading: 45% @ 12.34 MB/s", "speed": "12.34 MB/s"}`
- **Response**: ignored.

#### `POST /api/agent/status`

Full live telemetry, posted every 5s from a background goroutine while a task runs. Response can ask the agent to cancel.

- **Request body**:
  ```json
  {
    "phase": "downloading",
    "vpn_status": "up",
    "public_ip": "203.0.113.42",
    "download_speed": "45.23 MB/s",
    "upload_speed": "12.34 MB/s",
    "task_title": "Some Release Title",
    "request_id": 987654321,
    "disk_free_gb": 412.7,
    "disk_reserved_gb": 8.5,
    "files": [
      {
        "name": "episode01.mkv",
        "size": 1073741824,
        "transferred": 536870912,
        "percent": 50.0,
        "speed": "12.34 MB/s",
        "up_speed": "5.67 MB/s",
        "phase": "downloading",
        "peers": 7
      }
    ]
  }
  ```
  `phase` values the agent emits: `idle`, `downloading`, `uploading`, `seeding`, `processing`.

- **Response**:
  ```json
  { "ok": true, "cancel_request_id": 0 }
  ```
  If `cancel_request_id` matches the current `request_id`, the agent aborts the task cleanly.

---

### 4. Lifecycle — task completion

#### `POST /api/agent/complete`

Final report for a task. The agent sends it gzipped multipart and retries up to 3 times with 10s backoff (infinitely on maintenance).

- **Content-Encoding**: `gzip`
- **Form fields**:
  | Field | Type | Required | Notes |
  |-------|------|----------|-------|
  | `lock_id` | string (int) | yes | |
  | `request_id` | string (int64) | yes | |
  | `status` | string | yes | `completed`, `failed`, or `aborted`. |
  | `fail_reason` | string | when `status != completed` | Human-readable. |
  | `password` | string | no | Present if `ENCRYPT=true`. |
  | `nzb_data` | file | yes on success | The `.nzb` file. |
  | `media_info` | string (JSON) | yes on success | Serialized `VideoInfo` (codec, resolution, HDR, audio tracks, etc). |
  | `screenshot_N` | file | no | `screenshot_1`, `screenshot_2`, … JPEGs. |
- **Response**: `{"ok": true, "nzb_id": 1234567}`
- **Size cap**: payload (base + screenshots) is capped at ~80 MB. If larger, the agent sends `/complete` without screenshots and then uploads each screenshot separately via `/api/agent/screenshot`.
- **Status semantics**:
  - `completed` — release stored, request fulfilled.
  - `failed` — unrecoverable error in the release itself (fail the request).
  - `aborted` — agent-local condition (disk, VPN, cancel); just release the lock, don't blame the request.

#### `POST /api/agent/screenshot`

Oversized screenshots uploaded individually after a successful `/complete`.

- **Content-Encoding**: `gzip`
- **Form fields**:
  | Field | Type | Required |
  |-------|------|----------|
  | `nzb_id` | string (int64) | yes |
  | `screenshot` | file | yes |
- **Response**: ignored (non-fatal if it fails — a warning is logged).

---

### Minimal implementation checklist

To host an agent compatibly, a server must implement at least:

- Auth: issue bearer tokens bound to an agent identity.
- `POST /api/agent/poll` — hand out work with a `lock_id`.
- `POST /api/agent/complete` — accept the NZB and fulfil the request.
- `POST /api/agent/clear-locks` — expire locks on reconnect.
- `GET /api/agent/config` — return at least `{}`.
- `POST /api/agent/status` — at minimum return `{"ok": true}`.
- `POST /api/agent/log`, `POST /api/agent/progress` — can be no-ops returning `200`.

The rest (`/local-config`, `/directives`, `/directives/ack`, `/backfill`, `/screenshot`, `/torrent/...`, `/groups`) can be added incrementally; the agent tolerates `404` on the optional ones.

Advertise a minimum agent protocol by returning `426 Upgrade Required` with `{"min_protocol":N,"message":"..."}` whenever an older agent calls in.

---

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
