-- Relational schema v2. Fresh databases only; no legacy migration.

CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE artists (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_key TEXT NOT NULL UNIQUE
);

CREATE TABLE genres (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_key TEXT NOT NULL UNIQUE
);

CREATE TABLE albums (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_key TEXT NOT NULL,
  year INTEGER NOT NULL,
  artist_key TEXT NOT NULL,
  UNIQUE(name_key,year,artist_key)
);

CREATE TABLE sources (
  "folder_times" TEXT,
  "id" TEXT PRIMARY KEY,
  "name" TEXT,
  "path" TEXT,
  "keep" TEXT,
  "ignore" TEXT,
  "error" TEXT,
  active INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1))
);

CREATE TABLE tracks (
  "modified_at" INTEGER,
  "created_at" INTEGER,
  "id" TEXT PRIMARY KEY,
  "source_id" TEXT,
  "path" TEXT,
  "filename" TEXT,
  "folder" TEXT,
  "title" TEXT,
  "artist_raw" TEXT,
  "album_raw" TEXT,
  "album_artist_raw" TEXT,
  "album_id" TEXT,
  "genre_raw" TEXT,
  "year" INTEGER,
  "disc" INTEGER,
  "number" INTEGER,
  "duration" REAL,
  "bitrate" INTEGER,
  "sample_rate" INTEGER,
  "format" TEXT,
  "added_at" INTEGER,
  "modified" INTEGER,
  "size" INTEGER,
  "revision" TEXT,
  "favorite" INTEGER,
  "play_count" INTEGER,
  "last_played" INTEGER,
  "missing" INTEGER,
  "cover" TEXT,
  "has_cover" INTEGER,
  "artwork_revision" TEXT,
  "lyrics" TEXT,
  "tags" TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(tags) AND json_type(tags)='object'),
  "tag_version" INTEGER,
  "track_gain" REAL,
  "album_gain" REAL,
  "track_peak" REAL,
  "album_peak" REAL,
  FOREIGN KEY(source_id) REFERENCES sources(id),
  FOREIGN KEY(album_id) REFERENCES albums(id)
);

CREATE TABLE playlists (
  "id" TEXT PRIMARY KEY,
  "name" TEXT,
  "smart" INTEGER,
  "sort" TEXT,
  "desc" INTEGER
);

CREATE TABLE track_artists (
  track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
  artist_id TEXT NOT NULL REFERENCES artists(id),
  position INTEGER NOT NULL,
  PRIMARY KEY(track_id,artist_id)
);

CREATE TABLE album_artists (
  album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
  artist_id TEXT NOT NULL REFERENCES artists(id),
  position INTEGER NOT NULL,
  PRIMARY KEY(album_id,artist_id)
);

CREATE TABLE track_genres (
  track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
  genre_id TEXT NOT NULL REFERENCES genres(id),
  position INTEGER NOT NULL,
  PRIMARY KEY(track_id,genre_id)
);

CREATE TABLE playlist_items (
  playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  PRIMARY KEY(playlist_id,track_id),
  UNIQUE(playlist_id,position)
);

CREATE TABLE playlist_rules (
  playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  node_id INTEGER NOT NULL,
  parent_id INTEGER,
  position INTEGER NOT NULL,
  mode TEXT NOT NULL,
  field TEXT NOT NULL,
  op TEXT NOT NULL,
  value_json TEXT NOT NULL CHECK(json_valid(value_json)),
  source_id TEXT NOT NULL,
  recursive INTEGER NOT NULL,
  PRIMARY KEY(playlist_id,node_id),
  FOREIGN KEY(playlist_id,parent_id) REFERENCES playlist_rules(playlist_id,node_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX playlist_rule_root ON playlist_rules(playlist_id) WHERE parent_id IS NULL;

CREATE INDEX playlist_rule_children ON playlist_rules(playlist_id,parent_id,position);

CREATE TABLE rule_sets (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE conversion_rules (
  rule_set_id TEXT NOT NULL REFERENCES rule_sets(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  formats TEXT NOT NULL CHECK(json_valid(formats)),
  format TEXT NOT NULL,
  bitrate_op TEXT NOT NULL,
  bitrate INTEGER NOT NULL,
  sample_rate_op TEXT NOT NULL,
  sample_rate INTEGER NOT NULL,
  codec TEXT NOT NULL,
  output_bitrate INTEGER NOT NULL,
  output_sample_rate INTEGER NOT NULL,
  PRIMARY KEY(rule_set_id,position)
);

CREATE TABLE playback_sessions (
  id TEXT PRIMARY KEY,
  track_id TEXT REFERENCES tracks(id) ON DELETE CASCADE,
  played_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX tracks_album ON tracks(album_id,disc,number);

CREATE INDEX tracks_source_path ON tracks(source_id,path);

CREATE INDEX tracks_favorite ON tracks(favorite) WHERE favorite=1 AND missing=0;

CREATE INDEX tracks_recent ON tracks(last_played DESC) WHERE last_played>0;

CREATE INDEX tracks_play_count ON tracks(play_count);

CREATE INDEX tracks_added ON tracks(added_at DESC);

CREATE INDEX track_artists_artist ON track_artists(artist_id,track_id);

CREATE INDEX album_artists_artist ON album_artists(artist_id,album_id);

CREATE INDEX track_genres_genre ON track_genres(genre_id,track_id);

CREATE INDEX playlist_items_track ON playlist_items(track_id);

PRAGMA user_version=2;
