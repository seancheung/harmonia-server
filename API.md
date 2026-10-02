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
| GET | `/outputs` | OwnTone output devices or an explanatory error |
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

`deadline` is absolute Unix milliseconds. `{action:"timer",deadline:0,finish:true}` means end of current song; `finish:false` with zero cancels. Other actions: `play`, `pause`, `next`, `previous`, `select` (zero-based `index`), `seek` (`position` in milliseconds), `volume` (0–100), `repeat` (`off`/`all`/`single`), `shuffle`, `local`, `append` (`ids`, zero-based insertion `position`), `move` (`index`, `position`), `remove` (`index`), `clear`, and `pair` (`outputId`, `pin`).

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
