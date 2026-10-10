# HTTP API

Base path: `/api`. Request and response bodies are JSON except `stream` and `cover`. Authentication is `Authorization: Bearer <token>` when configured. Browser media endpoints also accept `?token=...`.

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/health` | Liveness |
| GET | `/config` | Sources, conversion rules and shared settings |
| GET | `/home` | Bounded home sections |
| GET | `/browse` | Paginated collections and songs |
| GET | `/library/version` | Current page invalidation version |
| GET | `/queue/query` | Ordered playback selection, maximum 100 IDs |
| POST | `/tracks/resolve` | Resolve metadata for at most 200 IDs |
| GET | `/playlists?type=all|normal|smart` | Filtered playlist summaries |
| POST | `/sources` | Add `{name, path, keep: [], ignore: []}` |
| PUT | `/sources/:id` | Update source name and path rules; root is immutable |
| DELETE | `/sources/:id` | Remove a source; preserve missing playlist entries |
| GET | `/scan` | `{running, processed, total, errors, finishedAt}` |
| POST | `/scan?force=true` | Full reread; omit `force` for incremental scan |
| PUT | `/tracks/:id/favorite` | `{favorite: true}` |
| POST | `/tracks/:id/played` | `{session, seconds, completed, seeked}` |
| GET | `/tracks/:id/stream?ruleSet=:id` | Original or selected conversion; HTTP ranges for completed files |
| GET | `/tracks/:id/cover` | Album artwork; 404 when absent |
| GET | `/tracks/:id/lyrics` | `{lyrics: "..."}` |
| DELETE | `/recent` | Clear last-played timestamps without resetting counts |
| POST | `/playlists` | Create ordinary or smart list |
| PUT | `/playlists/:id` | Rename or edit smart rules/sort |
| DELETE | `/playlists/:id` | Delete only the playlist |
| POST | `/playlists/:id/items` | `add`, `remove`, `move`, `clean` |
| GET | `/capabilities` | Scanned formats and supported conversion options |
| POST | `/rule-sets` | Create `{name, rules}` |
| PUT | `/rule-sets/:id` | Edit a rule set |
| DELETE | `/rule-sets/:id` | Delete a rule set |
| GET | `/cache` | `{used, limit, pending, active}` in bytes/counts |
| PUT | `/cache` | `{limit: 5368709120}` |
| DELETE | `/cache` | Clean inactive entries; mark active work pending |
| GET | `/outputs` | AirPlay 1 / 2 output devices; select at most one |
| GET | `/remote` | Actual remote player, queue/index and sleep timer |
| POST | `/remote` | Remote playback command |

## Browse query

```json
{
  "section": "songs",
  "search": "",
  "rule": {
    "mode": "any",
    "rules": [
      {"mode": "all", "rules": [
        {"field": "genre", "op": "contains", "value": "Jazz"},
        {"field": "year", "op": "gte", "value": 2020}
      ]},
      {"field": "favorite", "op": "eq", "value": true}
    ]
  },
  "sort": "addedAt",
  "desc": true,
  "page": 1,
  "pageSize": 50
}
```

Send this JSON as the URL-encoded `query` parameter of `GET /browse`. Page sizes are 1–100. Use `section:"playlists"` and `detail:"<playlist ID>"` to evaluate smart rules or retain ordinary playlist order, including missing entries. The response includes page metadata and collection groups as described below.

Operators: `contains`, `eq`, `ne`, `gt`, `gte`, `lt`, `lte`. Supported fields include `title`, `artist`, `album`, `albumArtist`, `genre`, `year`, `duration` (seconds), `favorite`, `playCount`, `addedAt` (Unix milliseconds), `disc`, `number`, `bitrate` (kbps), `sampleRate` (Hz), `format`, and `tag:<name>`.

Folder example:

```json
{"field":"folder","op":"eq","sourceId":"source-id","value":"Jazz/Live","recursive":false}
```

`ne` excludes that exact range. `value: ""` means the source root. Missing folders remain in stored rules rather than silently broadening the filter.

## Playlist examples

```json
{"name":"Evening","smart":false}
```

```json
{"name":"Recent jazz","smart":true,"rule":{"field":"genre","op":"contains","value":"Jazz"},"sort":"addedAt","desc":true}
```

Item operations:

```json
{"action":"add","ids":["track-a","track-b"]}
```

```json
{"action":"move","ids":["track-b"],"position":1}
```

`add` is all-or-nothing. `remove` takes IDs. `clean` removes only missing entries. Smart playlists reject manual edits.

## Conversion rule example

```json
{
  "name":"Portable",
  "rules":[{
    "format":"flac",
    "bitrateOp":"gt","bitrate":320,
    "sampleRateOp":"","sampleRate":0,
    "codec":"mp3","outputBitrate":192,"outputSampleRate":44100
  }]
}
```

Empty match fields are unrestricted. Opus output uses 48000 Hz. FLAC/WAV do not apply a lossy bitrate setting. Query `/capabilities` before rendering available outputs.

## Remote commands

Examples:

```json
{"action":"outputs","outputs":["speaker-id"]}
```

```json
{"action":"start","ids":["track-a","track-b"],"index":0,"ruleSet":"","gain":"album","preamp":0,"protect":true}
```

```json
{"action":"timer","deadline":1893456000000,"finish":true}
```

`deadline` is absolute Unix milliseconds. `{action:"timer",deadline:0,finish:true}` means end of current song; `finish:false` with zero cancels. Other actions: `play`, `pause`, `stop`, `next`, `previous`, `select` (zero-based `index`), `seek` (`position` in milliseconds), `volume` (0–100), `repeat` (`off`/`all`/`single`), `shuffle`, `local`, `append` (`ids`, zero-based insertion `position`), `move` (`index`, `position`), `remove` (`index`), `clear`, and `pair` (`outputId`, `pin`).

## Page-based clients

Modern clients do not request `/library` or `/library/changes`, including at startup.

- `GET /config`: sources, conversion rules, and shared settings. Tracks, playlists and folder timestamp maps are omitted.
- `GET /library/version`: `{version: "opaque revision"}` for lightweight change detection. A changed revision invalidates page caches, not a full-library download.
- `GET /home`: `{recent, frequent, unheard, artists, total}`. Each section contains at most eight entries. Unheard selection is stable while the library is unchanged.
- `GET /browse?query=<URL-encoded JSON>`: server-filtered, sorted and paginated songs or collection groups.
- `GET /queue/query?query=<URL-encoded JSON>`: `{ids: [...], limit: 100}`. Ignores page/pageSize and returns at most 100 playable IDs in selection order. Optional `start` begins at that track; an absent start track returns 404. Missing files are excluded.
- `POST /tracks/resolve` with `{ids: [...]}`: `{items: [...]}` in requested order, at most 200 IDs. Queue clients resolve only the server-limited selection.
- `GET /playlists?type=all|normal|smart`: list metadata and rules without ordered track IDs, suitable for pickers.

Browse query fields are `section`, `detail`, `preset`, `search`, `rule`, `sort`, `desc`, `page`, `pageSize`, `albums`, `disc`, and `recursive`. Sections are `songs`, `albums`, `artists`, `genres`, `folders`, `favorites`, `recent`, and `playlists`. `detail` is a collection ID; folders use `sourceId|relative/folder`. Presets are `recentlyAdded`, `topSongs`, `topArtists`, and `unheard`. Pages start at one, default to 50 entries, and allow sizes 1–100. Artist/genre details can request album groups with `albums:true`; folder queue requests can include descendants with `recursive:true`.

Browse responses include `items`, `groups`, `folders`, `total`, `trackCount`, `libraryCount`, `page`, `pageSize`, `heading`, `artist`, `year`, and `discs`. Album detail includes `cover`; playlist detail includes metadata in `playlist`. Group previews contain at most four tracks. Lyrics are fetched separately. Filtering precedes pagination; ordinary playlist order is retained.

Configuration, home, browse, version and playlist-list responses expose content ETags. Send `If-None-Match` to receive a bodyless 304 when unchanged. Responses use `Cache-Control: private, no-cache`; clients show their persisted page immediately and revalidate in the background. Cache keys must include the server, account/token and complete query. The server caches at most 120 page representations and 20 MB; committed library changes invalidate their revision. Authentication is checked on every request, including cache hits. Old `/library`, `/library/changes`, `/tracks/query`, and `/playlists/memberships` routes are removed (404, or 405 when the path matches another method). There is no compatibility fallback. Upgrade server and clients together.

### Actual playback audio information

`GET /tracks/{id}/audio-info` uses the same authentication and query parameters as
`GET /tracks/{id}/stream`: `ruleSet`, and for remote audio previews `output=airplay`, `gain`,
`preamp`, `protect`. Clients must send the same values used for playback. Auto gain
must first be resolved to `track` or `album`, as for streaming. The response is:

```json
{
  "trackId": "song-id",
  "revision": "song-revision",
  "audioIdentity": "sha256-identity",
  "transcoded": true,
  "source": {
    "codec": "flac", "container": "flac", "sampleRate": 96000,
    "channels": 2, "bitDepth": 24, "duration": 240,
    "bitrate": 2500000, "bitrateKind": "estimatedAverage",
    "bitrateSource": "containerSize"
  },
  "output": {
    "codec": "aac", "container": "aac", "sampleRate": 44100,
    "channels": 2, "duration": 240,
    "bitrate": 257000, "bitrateKind": "average",
    "bitrateSource": "audioStream"
  }
}
```

Numbers above are illustrative. `source` describes the original file; `output`
describes the actual file served for these playback parameters. A nonmatching
conversion rule leaves `transcoded=false`. Remote audio previews always normalize to stereo 44.1 kHz/16-bit WAV and include ReplayGain. This endpoint may generate the converted file, reusing the same
conversion cache and lock as streaming; it does not download audio to the client.
It probes actual bytes with ffprobe, never presents configured target bitrate as
actual bitrate. Unknown numeric fields are omitted. `sampleRate` is Hz; `bitrate`
is **bits per second**, unlike any existing library fields expressed in kbps.
`bitDepth` is reported for PCM/lossless codecs only, not the decoder sample format
of lossy audio. `average` means the audio stream's reported bitrate;
`estimatedAverage` is inferred from full container size / duration and includes
container overhead and embedded artwork. Neither indicates an instantaneous
bitrate or proves CBR. Clients should label estimates accordingly.

Responses use `Cache-Control: private, no-store`. Clients may persist the metadata
with their matching local audio cache and retain `revision` and `audioIdentity`
to distinguish outputs. An older local audio file must retain its saved metadata,
not be relabeled with a new server response after rules change. Missing tracks
return 404; failed conversion/probing returns 422 without speculative metadata.
This endpoint describes server audio bytes, not the final hardware/AirPlay output
format after device-side resampling.

`GET /remote` also includes `audioParameters: {ruleSet, gain, preamp, protect}`
from the server's saved remote queue, including responses that omit an unchanged
queue. Clients should use these values with `output=airplay` for remote audio
information, not their local playback preferences.

### Native AirPlay playback

`GET /outputs` discovers IPv4 AirPlay 1 / 2 receivers and returns
`{outputs: [{id, name, address, port, type, selected, requires_auth, unsupported_reason?}], protocols: ["airplay1", "airplay2"], maxSelected: 1}`.
Discovery errors are returned in `error`; an empty list is not a simulated device.

`POST /remote` keeps the queue/transport actions above. `outputs` accepts zero or one ID; multiple IDs are rejected with 400. Discover outputs before selecting one. `pair` is a two-step operation: send `{action:"pair", outputId:"...", pin:""}` to start, then send the same command with the displayed PIN within two minutes. Credentials are stored only on the server. A disconnected client does not cancel the server's active playback.

`start` validates tracks and stores the local queue; connection/decoding then proceed asynchronously. Inspect `GET /remote` for `player.state` (`loading`, `play`, `pause`, `stop`) and `error`. `play` is the scheduled audible timeline, not a hardware acknowledgement. Authentication, transport and decoder failures leave the queue paused with an error. Pause/seek/track changes close and recreate the one transport session. There is no automatic reconnect loop.

When AirPlay is enabled, `GET /remote` reports `configured:true`, meaning the native implementation is available, not that a receiver is connected. It adds `protocol:"airplay1"` or `protocol:"airplay2"` according to the selected output (defaults to AirPlay 2 without an output), `outputId`, and `transportFormat:{codec:"alac",sampleRate:44100,bitDepth:16,channels:2}`. Queue metadata is omitted when the supplied `queueVersion` matches. Existing `audioParameters` contains `ruleSet:""` plus the effective gain settings.

With `HARMONIA_DISABLE_AIRPLAY=true`, `GET /remote` returns `{configured:false, player:{state:"stop"}, queue:[]}` and `GET /outputs` returns `{outputs:[], protocols:[], maxSelected:0}` without discovery. `POST /remote` returns HTTP 503 with an explanatory error for all commands. `/capabilities` reports `airplay:false`, `airplayProtocols:[]`, and `airplayMaxOutputs:0`. Authentication rules remain unchanged.

Remote playback ignores the client's `ruleSet`: source audio is decoded directly with FFmpeg. `/tracks/{id}/stream?output=airplay` and the corresponding `audio-info` describe a normalized WAV preview with the same PCM format/gain, not the encrypted ALAC wire stream. Local conversion rules are unchanged.

The previous external-backend URL configuration, conversion-rule setting and settings endpoint have been removed. Old database settings are ignored. `remote.json` queue entries remain readable; external queue item IDs are ignored. Restart restores paused state without a network connection or an armed sleep timer.

Selecting an output stops the previous playback, preserves the queue, resets
position and sleep timers, and reads the selected receiver's volume without
sending RECORD or audio. `player.volume` is 0–100 or null when unreadable;
clients must not show an old output's volume for null. A temporary control/timing
SETUP may be needed to read initialVolume and is torn down immediately.
Starting playback preserves the receiver volume unless the user explicitly set
one while stopped. `stop` retains the queue and resets position to zero.

Discovery merges `_airplay._tcp` and `_raop._tcp` services by device ID and prefers
AirPlay 2. `type` identifies the selected protocol. Unsupported services
remain visible with `unsupported_reason`. They cannot be selected or paired;
clients should show the reason and disable their controls. Discovery includes
receivers running on the same IPv4 host using multicast loopback.

AirPlay 1 supports UDP ALAC with plaintext or RSA/AES encryption. Legacy PIN,
password and FairPlay-only modes are not supported; `pair` applies to AirPlay 2.
When AirPlay is enabled, the capabilities response exposes `airplayProtocols:["airplay1","airplay2"]`; disabling it returns an empty list.
