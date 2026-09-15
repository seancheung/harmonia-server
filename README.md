# Harmonia Server

A private, single-user music library server built with Go and SQLite. This directory is a standalone repository: all build commands, Docker contexts and GitHub Actions paths start here. It does not depend on a parent workspace or the web player's source code.

Repository: [seancheung/harmonia-server](https://github.com/seancheung/harmonia-server). Companion player: [seancheung/harmonia-web-player](https://github.com/seancheung/harmonia-web-player).

## Start with GHCR

```sh
docker pull ghcr.io/seancheung/harmonia-server:latest
docker run -d --name harmonia-server --restart unless-stopped \
  -p 8090:8090 \
  -v harmonia-data:/data \
  -v /absolute/path/to/music:/music:ro \
  -e HARMONIA_ORIGIN=http://localhost:8080 \
  ghcr.io/seancheung/harmonia-server:latest
```

For a source build, run `docker build -t harmonia-server .` and use `harmonia-server` instead of the GHCR image in the run command. Alternatively, set `MUSIC_PATH` in a local `.env` file and run `docker compose up -d --build`. The container includes FFmpeg/FFprobe and runs as UID/GID 10001. Grant that user read access to music and write access to a bind-mounted data directory, or use the named volume above.

The example allows the player at `http://localhost:8080`. For a different hostname or another computer, set `HARMONIA_ORIGIN` to the actual player origin and enter the browser-reachable server address in the player's connection settings.

### Image tags and updates

The workflow publishes `ghcr.io/seancheung/harmonia-server` for Linux amd64 and arm64 after checks pass. Pushes to `main` update both `:main` and `:latest`; a stable release tag such as `v1.2.3` produces `:1.2.3` and also updates `:latest`. Prerelease tags do not update `:latest`. Use an existing version tag or digest for a pinned deployment. Images are available only after a successful publishing workflow.

To update, pull the chosen image and recreate the container with the same ports, environment variables and volume mounts. Keep the `harmonia-data` volume to preserve library data.

Public GHCR packages require no login. For private packages, run `docker login ghcr.io -u YOUR_GITHUB_USERNAME` and authenticate with a personal access token (classic) with `read:packages` scope. The repository owner must configure package visibility for public pulls. See the [GHCR authentication documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

Open the web player, visit **Settings**, add `/music` as a source, and choose **Update library**. Paths entered in the player are paths on the server, not paths on the browser's computer. Multiple sources belong to the same library. Original files are read-only inputs.

## Deploy both services with Docker Compose

Create a separate deployment directory containing the following `compose.yaml`. No source checkout or local image build is required.

```yaml
name: harmonia

services:
  server:
    image: ghcr.io/seancheung/harmonia-server:latest
    restart: unless-stopped
    ports:
      - "8090:8090"
    environment:
      HARMONIA_ORIGIN: "http://${HARMONIA_HOST:-localhost}:8080"
      HARMONIA_TOKEN: "${HARMONIA_TOKEN:-}"
    volumes:
      - data:/data
      - type: bind
        source: ${MUSIC_PATH:?Set MUSIC_PATH in .env}
        target: /music
        read_only: true
        bind:
          create_host_path: false

  web-player:
    image: ghcr.io/seancheung/harmonia-web-player:latest
    restart: unless-stopped
    ports:
      - "8080:80"

volumes:
  data:
```

Create a `.env` file beside it:

```dotenv
MUSIC_PATH=/absolute/path/to/music
HARMONIA_HOST=localhost
HARMONIA_TOKEN=
```

Use an existing absolute music directory. On Windows with Docker Desktop, use a path such as `MUSIC_PATH=D:/Music`. The server runs as UID/GID 10001 and needs read access to the mounted music. The named `data` volume holds library metadata, artwork and cache.

For access from another computer, replace `localhost` with the Docker host's LAN IP or hostname, such as `HARMONIA_HOST=192.168.1.20`. This value has no scheme or port. If authentication is desired, set a shared token and enter the same token in the player.

Start both containers:

```sh
docker compose pull
docker compose up -d
docker compose ps
```

1. Open `http://localhost:8080` (or `http://192.168.1.20:8080` for the LAN example).
2. In **Settings → Server connection**, set the server URL to `http://localhost:8090` (or `http://192.168.1.20:8090`) and save. Do not append `/api`.
3. Add `/music` as a source and run **Update library**.

The static player makes API requests from the browser, so the server address must be reachable from that browser. Do not enter `http://server:8090`: the Compose service name is only resolvable inside Docker. This example uses separate HTTP ports; HTTPS deployments need an HTTPS API or a same-origin reverse proxy.

To update both services:

```sh
docker compose pull
docker compose up -d
```

To stop them, run `docker compose down`. Keep the deployment directory and project name unchanged so the same data volume is reused. Do not add `--volumes` unless you intend to delete the stored library data. Existing deployments with a different data volume must explicitly reuse that volume; this example does not migrate it.

AirPlay additionally requires an OwnTone instance. Add `HARMONIA_OWNTONE` and `HARMONIA_PUBLIC_URL` under the server's environment; the public URL must be reachable from OwnTone, for example `http://192.168.1.20:8090`.

## Native development

Requirements: Go 1.27 and a full FFmpeg build with `ffmpeg` and `ffprobe` on `PATH`. Some minimal FFmpeg distributions contain no encoders; `/api/capabilities` reports the actual output encoders available.

```sh
go mod download
go run ./cmd/harmonia
```

The default API is `http://localhost:8090/api`. SQLite, extracted artwork, completed transcodes and remote playback state live under `./data`.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `HARMONIA_LISTEN` | `:8090` | HTTP listen address |
| `HARMONIA_DATA` | `./data` (`/data` in Docker) | Writable application data |
| `HARMONIA_CACHE` | `<HARMONIA_DATA>/cache` | Dedicated writable transcode cache directory |
| `HARMONIA_FFMPEG` | `ffmpeg` | Full FFmpeg executable path |
| `HARMONIA_FFPROBE` | `ffprobe` | FFprobe executable path |
| `HARMONIA_ORIGIN` | `http://localhost:5173,http://127.0.0.1:5173` | Comma-separated permitted browser origins |
| `HARMONIA_TOKEN` | empty | Optional shared bearer token |
| `HARMONIA_OWNTONE` | empty | OwnTone base URL, e.g. `http://192.168.1.20:3689` |
| `HARMONIA_PUBLIC_URL` | `http://localhost:8090` | Server URL reachable **from OwnTone** |

This service is intended for a private network. For exposure outside it, configure the shared token and terminate HTTPS at a reverse proxy. Enter the same token in the player's connection settings. Audio and artwork URLs accept a `token` query parameter for browser media elements and OwnTone; avoid logging query strings at a reverse proxy. There are no user accounts or permissions.

## Library behavior

- Scanning is manual. Ordinary scans inspect file size and nanosecond modification time; full scans reread every allowed file and invalidate its conversion generation.
- Source-relative paths identify existing files. Tag edits preserve ID, date added, favorites, counts and playlist membership. Renames and moves create a new ID. Removed or excluded tracks become tombstones so ordinary playlists can display and clean missing entries.
- An unreadable source records an error without deleting its existing collection. A scan reports failures and never retries itself.
- Per-source keep/ignore globs use `/`, `*`, `**` and `?`. Ignore takes priority. Empty lines are discarded. Changing rules only takes effect at the next scan.
- FFprobe reads tags, stream format, duration, sample rate, bitrate and ReplayGain. Artists and genres split only on semicolons.
- Album identity normalizes album name, sorted album-artist members and year. Missing album artists fall back to track artists, then an unknown artist. Missing years remain distinct.
- Directory covers are checked independently of audio modification times. The order is `cover`, `folder`, `front`, with JPG, JPEG, PNG within each name. All album directories are checked before embedded artwork. Corrupt images fall through to the next candidate. Artwork content hashes refresh browser image URLs.
- Local `.lrc` or `.txt` sidecars take priority over embedded lyric text. No artwork or lyrics are downloaded.

## Filtering, playlists and history

`POST /api/tracks/query` validates nested `all`/`any` groups, up to 30 total nodes and four group levels. Folder rules match a source ID plus a full directory path and an optional recursive flag. Search covers music metadata, excluding file paths and filenames. Sorting is stable, with missing values always last. Pagination happens after filtering and sorting; `all: true` requests the complete playback snapshot.

Ordinary playlists store ordered IDs. An addition containing any duplicate fails atomically. The `move` action uses a one-based position in the complete list. Smart playlists store the same rule tree plus a sort field and direction, and evaluate against current metadata, favorites and counts.

Playback reports use an idempotency session ID. A session counts at 15 seconds of actual listening, or after an unseeked complete track shorter than 15 seconds. The browser measures played time rather than submitted playhead position. Recently played retains 500 distinct tracks; clearing it does not reset counts. This is a trusted single-user accounting API, not a fraud-resistant analytics service.

## Conversion and caching

Set `HARMONIA_CACHE` to place transcodes on a separate disk. In Docker, mount a dedicated volume at `/cache` and set `HARMONIA_CACHE=/cache`; UID 10001 needs write access. Use a directory exclusively for Harmonia's transcode cache because cache cleanup manages its contents. Restart after changing this setting. Existing cache files are not moved automatically. SQLite, artwork and remote state remain under `HARMONIA_DATA`.

Rule sets are ordered. Every configured condition must match; the first matching rule selects output codec, bitrate in kbps and sample rate in Hz. No selection or no match serves the original file directly with HTTP range support. Capabilities come from installed FFmpeg encoders. A conversion failure is reported, not silently retried using another codec.

The persistent cache defaults to 5 GiB and evicts least-recently-used inactive entries. A key includes the track generation, file signature and actual output settings, not the rule-set name. Browser transcodes never contain ReplayGain. AirPlay transcodes additionally distinguish gain settings. In-progress files are never considered complete cache entries. Active files survive manual cleanup and are removed on release. When a result does not fit the cache, it is served from a temporary file and removed after the request; conversion currently finishes before that file is served, so long files can have an initial preparation delay.

## AirPlay through OwnTone

Queue additions use batches of at most 20 URLs and a 6,000-character encoded request target. Queue reads use pages of 100 items. Replacing a queue snapshots the existing OwnTone items and playback position before clearing; failed additions attempt to remove partial batches and restore the previous queue and position. Recovery errors are reported explicitly when OwnTone remains unavailable. Queue commands are serialized with background polling. Remote status responses include full queue metadata only when the client's queue version differs (clients without a version still receive the full queue).

Harmonia uses the [OwnTone JSON API](https://owntone.github.io/owntone-server/json-api/) for device discovery, PIN pairing, multiple outputs and independent device playback. OwnTone must run on a system and network capable of discovering the speakers. Windows Chrome does not need native AirPlay support.

1. Install and configure OwnTone on the speaker network.
2. Set `HARMONIA_OWNTONE` to its base URL.
3. Set `HARMONIA_PUBLIC_URL` to an address OwnTone can reach. `localhost` is only correct if both processes share that network namespace; Docker Desktop commonly uses `http://host.docker.internal:8090`.
4. Choose devices in the player's output dialog and complete any required PIN pairing.

OwnTone requests Harmonia stream URLs. Server-side ReplayGain is applied once to these outputs, independently of the chosen conversion rule set; the browser's local gain path is stopped. Remote queue and timer state survive closing the page. Finish-current-track timers temporarily leave only the current item in OwnTone, while preserving the complete Harmonia queue for restoration. Device codec support and transport determine AirPlay gapless behavior. Physical speaker pairing, multiroom synchronization and device-specific behavior require acceptance testing with the target hardware; they are not simulated as successful when OwnTone is unavailable.

## API overview

See [API.md](API.md) for request bodies and examples. JSON errors use `{ "error": "message" }` and unsuccessful HTTP status codes. Mutations commit before returning success. `GET /api/health` is unauthenticated; all other routes honor `HARMONIA_TOKEN` when set.

## Persistence and implementation

The SQLite WAL database stores one library document and commits each mutation atomically. In-memory snapshots are deep copied and protected by a read/write lock. This keeps playlist, source and metadata transitions consistent, but both document writes and full-library browser bootstrap are proportional to library size. Large-library deployments should benchmark their collection before expanding this storage model to indexed relational tables.

Music sources are never modified. Only application-owned files under the data and cache directories are written or cleaned. There is no background filesystem watcher, online metadata service, offline playback cache, backup workflow or multi-user model.

## Validation and CI

```sh
go test ./...
go vet ./...
docker build --target test -t harmonia-server:test .
```

The Docker test stage runs `go test -race -cover ./...` and `go vet ./...` with full FFmpeg support. Native integration tests skip when that support is absent. Tests cover grouped filters, sort boundaries, album identity, path globs, atomic playlist edits, missing entries, history, scanner identity, artwork refresh, cache leases and OwnTone timer commands.

GitHub Actions tests pull requests. Pushes to `main` and `v*` tags additionally build and publish linux/amd64 and linux/arm64 images to `ghcr.io/seancheung/harmonia-server`. Copy this directory as the repository root; no root-level workspace configuration is needed.

Folder views can sort files and directories by filename, filesystem modification time, or filesystem creation time. Run an incremental scan after upgrading to populate timestamps. Windows uses CreationTime; Linux uses statx birth time when supported by the filesystem. Unsupported creation times remain unknown and sort last in either direction; inode change time and library added time are not substituted.


### Native multi-value tags

The server preserves repeated Vorbis Comment fields in native FLAC and Ogg Vorbis/Opus files, and null-separated ID3v2.4 text values in MP3 files. Values are exposed as arrays in `tagValues`; existing text fields remain available as semicolon-separated display values for compatible grouping, search and clients. Artist, album artist and genre values are split first at native boundaries, then at semicolons and any extra characters configured in server settings. A slash inside a native value is preserved unless `/` is configured.

The next ordinary library scan rereads metadata created by older versions once, preserving track IDs, favorites and play counts. Music files are not modified. Other tag formats continue to use ffprobe metadata; encrypted ID3 text frames or malformed native metadata produce a scan error and retain the previous library entry. Native metadata reads are bounded to 64 MiB.
