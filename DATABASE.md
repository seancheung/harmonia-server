# Database and update model

The database is a relational SQLite database (`harmonia.sqlite`, schema version 2), not a serialized library document. Canonical DDL is in [`internal/app/schema.sql`](internal/app/schema.sql); disposable lookup tables and indexes are defined in [`internal/app/search_schema.sql`](internal/app/search_schema.sql) and `store_search.go`.

## Entities and relationships

| Table | Purpose |
|---|---|
| `sources` | Music roots, scan metadata, include/exclude configuration; inactive roots retain references for missing tracks |
| `tracks` | One row per file: identity, location, audio properties, original metadata, favorite, play count, last played |
| `albums` | Unique album name key + year + album artist set key |
| `artists` | One entity per trimmed, case-insensitive name |
| `genres` | One entity per trimmed, case-insensitive genre name |
| `track_artists` | Track artists and their display order |
| `album_artists` | Album artists and their display order |
| `track_genres` | Track genre membership and order |
| `playlists` | Playlist name, kind and sort configuration |
| `playlist_items` | Manual playlist membership and explicit ordering |
| `playlist_rules` | Smart playlist rule tree: parent/child nodes, group mode, field, operator and scalar value |
| `rule_sets` | Named audio conversion configurations |
| `conversion_rules` | Ordered conversion rules with separate format, bitrate, sample-rate and codec columns |
| `playback_sessions` | Durable playback-report deduplication and report time |
| `settings` | Individual server/library settings |
| `track_search` | Derived case-folded text, BPM/key, and stable sorting values |
| `track_tag_index` | Derived per-value tag lookup with tag-name/value and nonempty indexes |

Foreign keys enforce associations; membership rows cascade on deletion. Indexes cover albums, source/path, favorites, recent playback, play count, date added, reverse artist/genre membership and playlist membership. SQLite WAL is enabled.

The `*_raw` track columns preserve the original artist/album/genre labels. Entity identity and relationships live in the entity and association tables. JSON is confined to extensible tag maps, configuration arrays/maps, and scalar rule values; entire tracks, playlists and the library are not stored as JSON objects.

### Identity rules

- Same artist name means the same artist. Trim surrounding whitespace and ignore case; do not merge aliases or simplified/traditional spellings.
- Album identity is the normalized album name, year, and set of normalized album artists. Artist order does not affect identity; the association retains display order.
- Missing album artist falls back to track artists, then `Unknown artist`.
- Unknown year is `0`; different years produce different albums.
- A missing album name yields a null album association. Singles keep their own artwork grouping and do not form one artificial album.
- Different file paths remain separate tracks. Disc and track numbers belong to the track.

## Writes and concurrency

- Favorite changes execute an indexed `UPDATE tracks SET favorite=... WHERE id=...`.
- Playback counting updates the track, deduplication session and bounded recent history in one transaction.
- Playlist and conversion settings save only their own metadata and association rows. Smart membership is not materialized during a save.
- Scanner/bulk updates compare records and write changed rows, including changed associations. They do not rewrite the entire database.
- Writers are serialized. A committed immutable in-memory snapshot is published only after SQLite commits; readers can read the previous committed snapshot while a write is pending. API responses still expose the library DTO, assembled from relational rows at startup.
- The library endpoint excludes private lyrics/artwork paths and playback-session state before copying its response. Single-track and remote-queue reads use the track index instead of copying the whole library.

## Tags and smart queries

`tracks.tags` is the sole extensible metadata field, with the same shape in Go, JSON API responses and SQLite:

```json
{"artist": ["Artist A", "Artist B"], "mood": ["Calm", "Happy"], "bpm": ["128"]}
```

Even a single value is an array. There is no separate `tag_values` column or `tagValues` API property. SQLite stores JSON as `TEXT` with a JSON validity/object constraint; it does not require a separate JSON column type. The scanner preserves native value boundaries and wraps ffprobe fallback values in arrays. Display strings are derived when needed.

The internal SQL query engine compiles validated rules to parameterized SQL. Common properties (favorite, year, play count, etc.) query ordinary columns. Arbitrary `tag:*` rules query `track_tag_index`, binding both the tag name and comparison value. Equality/range queries use `(tag_name,value_key,track_id)`; empty/nonempty queries use `(tag_name,nonempty,track_id)`. No user-controlled SQL is interpolated. Positive tag conditions match any value; `ne` and `notContains` require no value to match. A missing array or one containing only blank strings is empty. BPM/key aliases use the first valid/nonempty value in alias and array order. Unicode case folding matches Go semantics.

The JSON object remains the source of truth. `track_search` and `track_tag_index` are rebuildable projections, updated in the same transaction as scanned/edited track metadata and deleted through foreign-key cascades. Favorite and playback-counter updates do not rebuild them. On startup, missing structures are created and backfilled from existing schema-v2 tracks without deleting library data or requiring a rescan; subsequent starts do not reindex existing records. This is not support for the older single-row library format.

BPM/key are extracted once on metadata writes. Normalized artist/album/album-artist/genre/title conditions use indexed text columns, preserving the existing whole-display-string semantics (an exact genre rule does not silently become membership in one genre). Years and playback-session foreign keys have indexes. Full sort indexes preserve empty-last ordering, ties and both directions for added date/title; unfiltered lists walk those indexes. Filtered queries let SQLite choose the selective index and may sort their smaller result. Arbitrary substring matching still scans values for the selected tag or candidate tracks; ordinary B-tree indexes do not accelerate arbitrary substring matching.

The page HTTP API uses immutable in-memory metadata for grouping, filtering and ordering, and returns only bounded page results. Serialized responses are cached per committed library revision. The internal SQL query engine and its index benchmarks remain available for further query optimization; the old HTTP query and membership endpoints have been removed.

Query-plan tests verify selective indexes and absence of temporary sorting for unfiltered common sorts. `go test ./internal/app -run '^$' -bench '^BenchmarkIndexedLibrary$' -benchmem` compares JSON scanning with tag lookup, and full-result decoding with SQL pagination, on 8,000 synthetic tracks.

## Frontend updates

Favorites update immediately. Requests are ordered independently per track; the latest failed change rolls back to the last server-confirmed value. Older responses and library reloads cannot overwrite a newer pending choice.

Playlist saves refresh small summaries and invalidate relevant page responses. Smart playlist membership is evaluated on the server when its detail page or playback selection is requested. Clients do not download full membership maps or full music-library snapshots. Page caches are isolated by server, credentials and query, and use ETag revalidation.

## Fresh initialization

There is intentionally **no migration** from the old single-row `library` schema. Old databases are rejected with an explicit startup error. A deployment of this revision needs a fresh data database and a rescan; do not reuse an old database expecting automatic migration.

With the server stopped and a fresh data directory:

```sh
go run ./cmd/harmonia --init-db
```

This loads the usual `.env` / `HARMONIA_DATA` configuration, creates the schema and exits without starting playback or HTTP services. A normal server start also initializes a fresh database. When resetting an existing installation, also clear its saved `remote.json` queue because its track IDs refer to the old library. Music source files are not modified.

Schema changes are explicit in `schema.sql`; bump the schema version when introducing a future incompatible layout. Backups should use SQLite's backup facilities or be taken with the server stopped so WAL contents are included consistently.
