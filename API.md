# HTTP API

Base path: `/api`. Request and response bodies are JSON except `stream` and `cover`. Authentication is `Authorization: Bearer <token>` when configured. Browser media endpoints also accept `?token=...`.

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/health` | Liveness |
| GET | `/library` | Library snapshot: sources, tracks, playlists, rule sets |
| POST | `/sources` | Add `{name, path, keep: [], ignore: []}` |
| PUT | `/sources/:id` | Update source name and path rules; root is immutable |
| DELETE | `/sources/:id` | Remove a source; preserve missing playlist entries |
| GET | `/scan` | `{running, processed, total, errors, finishedAt}` |
| POST | `/scan?force=true` | Full reread; omit `force` for incremental scan |
| POST | `/tracks/query` | Filter, sort, page, or request a complete playback snapshot |
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

## Track query

```json
{
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

Response: `{items: Track[], total, page, pageSize}`. Page sizes are 25, 50 or 100. `all: true` returns the complete ordered result. `playlistId` evaluates a saved smart list or preserves an ordinary list's manual order, including missing entries.

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
