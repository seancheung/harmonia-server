# Harmonia Server

A private, single-user music library server built with Go and SQLite. This directory is a standalone repository: all build commands, Docker contexts and GitHub Actions paths start here. It does not depend on a parent workspace or the web player's source code.

Repository: [seancheung/harmonia-server](https://github.com/seancheung/harmonia-server). Companion player: [seancheung/harmonia-web-player](https://github.com/seancheung/harmonia-web-player).

## Relational database reset

This revision uses a new relational schema and intentionally does not migrate the previous single-row library database. Deploy with a fresh database and rescan your sources. See [DATABASE.md](DATABASE.md) for the schema, identity rules, transaction model and initialization command.

## Start with GHCR

These examples target Docker Engine on Linux with host networking for AirPlay. Publishing only HTTP through a bridge is insufficient for multicast discovery and receiver-initiated UDP timing/control traffic. Do not add `-p` / `ports` to the server in host mode.

```sh
docker pull ghcr.io/seancheung/harmonia-server:latest
docker run -d --name harmonia-server --restart unless-stopped \
  --network host \
  -v harmonia-data:/data \
  -v /absolute/path/to/music:/music:ro \
  -e HARMONIA_ORIGIN=http://localhost:8080 \
  ghcr.io/seancheung/harmonia-server:latest
```

For a source build, run `docker build -t harmonia-server .` and use `harmonia-server` instead of the GHCR image in the run command. Alternatively, set `MUSIC_PATH` in a local `.env` file and run `docker compose up -d --build`. The container includes FFmpeg/FFprobe. If you override the container user, grant it read access to music and write access to data/cache directories, plus permission to bind UDP 319/320 for AirPlay PTP.

The example allows the player at `http://localhost:8080`. For a different hostname or another computer, set `HARMONIA_ORIGIN` to the actual player origin and enter the browser-reachable server address in the player's connection settings.

### Image tags and updates

The workflow publishes `ghcr.io/seancheung/harmonia-server` for Linux amd64 and arm64 after checks pass. Pushes to `main` update both `:main` and `:latest`; a stable release tag such as `v1.2.3` produces `:1.2.3` and also updates `:latest`. Prerelease tags do not update `:latest`. Use an existing version tag or digest for a pinned deployment. Images are available only after a successful publishing workflow.

To update, pull the chosen image and recreate the container with the same network mode, environment variables and volume mounts. Keep the `harmonia-data` volume to preserve library data.

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
    network_mode: host
    environment:
      HARMONIA_LISTEN: "${HARMONIA_LISTEN:-:8090}"
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
HARMONIA_LISTEN=:8090
HARMONIA_TOKEN=
```

Use an existing absolute music directory readable by the container user. The named `data` volume holds library metadata, artwork and cache.

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

AirPlay 1 / 2 audio is sent directly by Harmonia to one receiver on its LAN. Both the supplied Compose file and the examples above use Linux host networking. Allow mDNS discovery and receiver-initiated UDP timing/control traffic through the host firewall, including UDP 319/320 for PTP; those ports must be available. The web player can remain on a bridge network.

Docker Desktop host networking is an opt-in feature with different limitations from Docker Engine on Linux; see the [Docker host networking documentation](https://docs.docker.com/engine/network/drivers/host/). AirPlay discovery and playback on Docker Desktop require separate verification. For AirPlay on Windows/macOS, use the native server or a Linux host with direct LAN access. For browser-only playback, bridge networking with HTTP port publishing is an option, but does not provide the AirPlay connectivity described here.

## Native development

Requirements: Go 1.27 and a full FFmpeg build with `ffmpeg` and `ffprobe` on `PATH`. Some minimal FFmpeg distributions contain no encoders; `/api/capabilities` reports the actual output encoders available.

```sh
go mod download
go run ./cmd/harmonia
```

The default API is `http://localhost:8090/api`. SQLite, extracted artwork, completed transcodes and remote playback state live under `./data`.

Run from the `harmonia-server` directory to automatically load its optional `.env` file at startup. Existing process environment variables take precedence, including explicitly empty values. The file supports literal `KEY=VALUE` entries, optional single/double quotes, comments, and an optional `export` prefix; shell expansion is not performed. Restart `go run ./cmd/harmonia` after editing it. Docker Compose users must run `docker compose up -d` to apply changed environment settings; `docker compose restart` retains the old container environment.

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

This service is intended for a private network. For exposure outside it, configure the shared token and terminate HTTPS at a reverse proxy. Enter the same token in the player's connection settings. Audio and artwork URLs accept a `token` query parameter for browser media elements; avoid logging query strings at a reverse proxy. There are no user accounts or permissions.

### Custom HTTP port and container health

Set `HARMONIA_LISTEN=:9090` in the Compose `.env` file (or pass `-e HARMONIA_LISTEN=:9090` to `docker run`) and recreate the container. In host mode this changes the host port directly; no port mapping is needed. Update the player's server URL to `http://<server-host>:9090`. `HARMONIA_ORIGIN` remains the web player's origin, not the API address.

The image health check reads `HARMONIA_LISTEN` at runtime and calls the unauthenticated `/api/health` endpoint. Empty/unset values default to `:8090`, matching the server. Wildcard listeners (`:9090`, `0.0.0.0:9090`, `[::]:9090`) are checked through loopback; explicit bind addresses are checked as configured. HTTP health does not verify AirPlay discovery or receiver connectivity.

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

`GET /api/browse` validates nested `all`/`any` groups, up to 30 total nodes and four group levels. Folder rules match a source ID plus a full directory path and an optional recursive flag. Search covers music metadata and filenames, excluding server file paths. Sorting is stable, with missing values always last. Pagination happens after filtering and sorting; playback selections use `/api/queue/query` with a 100-song limit.

Ordinary playlists store ordered IDs. An addition containing any duplicate fails atomically. The `move` action uses a one-based position in the complete list. Smart playlists store the same rule tree plus a sort field and direction, and evaluate against current metadata, favorites and counts.

Playback reports use an idempotency session ID. A session counts at 15 seconds of actual listening, or after an unseeked complete track shorter than 15 seconds. The browser measures played time rather than submitted playhead position. Recently played retains 500 distinct tracks; clearing it does not reset counts. This is a trusted single-user accounting API, not a fraud-resistant analytics service.

## Conversion and caching

Set `HARMONIA_CACHE` to place transcodes on a separate disk. In Docker, mount a dedicated volume at `/cache` and set `HARMONIA_CACHE=/cache`; the container user needs write access. Use a directory exclusively for Harmonia's transcode cache because cache cleanup manages its contents. Restart after changing this setting. Existing cache files are not moved automatically. SQLite, artwork and remote state remain under `HARMONIA_DATA`.

Rule sets are ordered. Every configured condition must match; the first matching rule selects output codec, bitrate in kbps and sample rate in Hz. No selection or no match serves the original file directly with HTTP range support. Capabilities come from installed FFmpeg encoders. A conversion failure is reported, not silently retried using another codec.

The persistent cache defaults to 5 GiB and evicts least-recently-used inactive entries. A key includes the track generation, file signature and actual output settings, not the rule-set name. Browser transcodes never contain ReplayGain. AirPlay preview transcodes additionally distinguish gain settings and channel count. In-progress files are never considered complete cache entries. Active files survive manual cleanup and are removed on release. When a result does not fit the cache, it is served from a temporary file and removed after the request; conversion currently finishes before that file is served, so long files can have an initial preparation delay.

## Native AirPlay 1 / 2 (single receiver)

Harmonia owns the remote queue, playback clock and FFmpeg decoder and sends audio directly over a native AirPlay 1 (RAOP) or AirPlay 2 session. No external AirPlay daemon or AirPlay library is used. The Go implementation lives in `internal/airplay`; `golang.org/x/crypto` supplies cryptographic primitives only. AirPlay 2 is preferred when both services are advertised. Multiroom is not supported.

1. Run Harmonia on the receiver's IPv4 LAN and open **Playback devices** in the web player.
2. Select one discovered AirPlay output. Discovery uses the service's advertised port and capabilities, not model-name rules.
3. For protected outputs, click **Pair** to initiate pairing, then enter the PIN. Credentials and the sender identity are stored in `airplay-identity.json` with mode 0600.
4. Start a track. `loading` covers connection, authentication and initial buffering; `play` describes the sender's scheduled audible timeline, not receiver-confirmed sound.

AirPlay 1 supports plaintext or RSA/AES encrypted RAOP over UDP; password/PIN and FairPlay-only legacy receivers are not yet supported. AirPlay 2 retains HAP authentication and encrypted transport. The current transport is realtime ALAC, stereo 44.1 kHz/16-bit, with verbatim ALAC frames. FFmpeg decodes source files and applies ReplayGain once; client conversion rules do not control the transport format. Queue changes happen locally without URL submission or a second media server. Pause, seek and track changes recreate the single session; sample-accurate gapless transitions are not yet provided. Closing a page does not stop the server. Queue/output/position persist, but a server restart restores paused state and requires discovery before reconnecting. Connection failures stop playback with an explicit error; retries are user-initiated.

The sender implements transient HAP and persistent PIN pairing, encrypted RTSP/event channels, RTP audio/retransmission, NTP timing and a unicast PTP sender clock selected from advertised capabilities. For PTP, UDP 319/320 must be available and bindable by the server (Linux deployments may require `CAP_NET_BIND_SERVICE`). The receiver must be able to reach the server's negotiated UDP timing/control ports. Merely exposing HTTP port 8090 through a Docker bridge is insufficient. IPv6-only receivers, peer-to-peer/AWDL discovery, buffered type-103 audio, receiver-clock following and MediaRemote now-playing UI are outside this first implementation.

This is an experimental native sender. Automated tests exercise an in-process receiver over real loopback TCP/UDP, including pairing, authenticated control/audio, event acknowledgement and retransmission. They do not establish interoperability with physical receivers or their firmware. Hardware playback and PTP interoperability still require acceptance testing. Refer to `AIRPLAY.md` for implementation boundaries and the test procedure.

## API overview

See [API.md](API.md) for request bodies and examples. JSON errors use `{ "error": "message" }` and unsuccessful HTTP status codes. Mutations commit before returning success. `GET /api/health` is unauthenticated; all other routes honor `HARMONIA_TOKEN` when set.

## Persistence and implementation

The SQLite WAL database stores normalized relational data and commits mutations atomically. Page APIs read immutable in-memory state, filter and group on the server, and return bounded pages. Clients no longer bootstrap full-library snapshots. Uncached grouping still scans library metadata; benchmark unusually large collections. Serialized page responses are cached by library revision with a 120-entry, 20 MB limit.

Music sources are never modified. Only application-owned files under the data and cache directories are written or cleaned. There is no background filesystem watcher, online metadata service, offline playback cache, backup workflow or multi-user model.

## Validation and CI

```sh
go test ./...
go vet ./...
docker build --target test -t harmonia-server:test .
```

The Docker test stage runs `go test -race -cover ./...` and `go vet ./...` with full FFmpeg support. Native integration tests skip when that support is absent. Tests cover grouped filters, sort boundaries, album identity, path globs, atomic playlist edits, missing entries, history, scanner identity, artwork refresh, cache leases and native AirPlay queue/timer commands.

GitHub Actions tests pull requests. Pushes to `main` and `v*` tags additionally build and publish linux/amd64 and linux/arm64 images to `ghcr.io/seancheung/harmonia-server`. Copy this directory as the repository root; no root-level workspace configuration is needed.

Folder views can sort files and directories by filename, filesystem modification time, or filesystem creation time. Run an incremental scan after upgrading to populate timestamps. Windows uses CreationTime; Linux uses statx birth time when supported by the filesystem. Unsupported creation times remain unknown and sort last in either direction; inode change time and library added time are not substituted.


### Native multi-value tags

The server preserves repeated Vorbis Comment fields in native FLAC and Ogg Vorbis/Opus files, and null-separated ID3v2.4 text values in MP3 files. Values are exposed and stored as arrays in the single `tags` map, including single-value tags; artist/album/genre display columns remain available for grouping and search. Artist, album artist and genre values are split first at native boundaries, then at semicolons and any extra characters configured in server settings. A slash inside a native value is preserved unless `/` is configured.

The next ordinary library scan rereads metadata created by older versions once, preserving track IDs, favorites and play counts. Music files are not modified. Other tag formats continue to use ffprobe metadata; encrypted ID3 text frames or malformed native metadata produce a scan error and retain the previous library entry. Native metadata reads are bounded to 64 MiB.

### Waveform cache

`GET /api/tracks/{id}/waveform` generates 512 amplitude peaks on demand using FFmpeg. Results are cached under `HARMONIA_DATA/waveforms`, with one replaceable JSON file per track. Changes to the track revision or source file invalidate its cached waveform. Generation is serialized, streams decoded samples without retaining the full audio, and has a 90-second timeout. These small files are separate from the transcoding cache limit. No waveforms are generated during library scans.

### Playlist list API

`GET /api/playlists?type=all|normal|smart` returns `{ "playlists": [...] }` without music-library tracks. Omitted or empty `type` means `all`; unknown values return HTTP 400. An empty result is `[]`. Playlist summaries retain smart rules but omit ordered track IDs. Existing authentication applies.

### Page APIs

Current Web and iOS clients load `/api/config`, `/api/home` and paginated `/api/browse` responses with ETag revalidation. `/api/library/version` detects changes without downloading tracks. `/api/queue/query` caps Play all at 100 songs on the server. Playlist summaries omit membership IDs. See [API.md](API.md#page-based-clients) for query fields, limits and cache behavior. The old full-library, incremental-sync, track-query and membership endpoints have been removed. Upgrade both clients together with this server.
