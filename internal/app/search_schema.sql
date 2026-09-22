-- Derived, rebuildable lookup data. tracks.tags remains the metadata source.
CREATE TABLE IF NOT EXISTS track_search (
  track_id TEXT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
  search_title TEXT NOT NULL,
  search_artist TEXT NOT NULL,
  search_album TEXT NOT NULL,
  search_album_artist TEXT NOT NULL,
  search_genre TEXT NOT NULL,
  search_key TEXT NOT NULL,
  search_bpm REAL,
  search_added_at INTEGER NOT NULL,
  search_disc INTEGER NOT NULL,
  search_number INTEGER NOT NULL,
  search_title_empty INTEGER NOT NULL,
  search_artist_empty INTEGER NOT NULL,
  search_album_empty INTEGER NOT NULL,
  search_album_artist_empty INTEGER NOT NULL,
  search_genre_empty INTEGER NOT NULL,
  search_key_empty INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS track_tag_index (
  track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
  tag_name TEXT NOT NULL,
  position INTEGER NOT NULL,
  value_key TEXT NOT NULL,
  nonempty INTEGER NOT NULL,
  PRIMARY KEY(track_id,tag_name,position)
);
CREATE INDEX IF NOT EXISTS track_tag_lookup ON track_tag_index(tag_name,value_key,track_id);
CREATE INDEX IF NOT EXISTS track_tag_nonempty ON track_tag_index(tag_name,nonempty,track_id);
CREATE INDEX IF NOT EXISTS track_search_title ON track_search(search_title,track_id);
CREATE INDEX IF NOT EXISTS track_search_artist ON track_search(search_artist,track_id);
CREATE INDEX IF NOT EXISTS track_search_album ON track_search(search_album,track_id);
CREATE INDEX IF NOT EXISTS track_search_album_artist ON track_search(search_album_artist,track_id);
CREATE INDEX IF NOT EXISTS track_search_genre ON track_search(search_genre,track_id);
CREATE INDEX IF NOT EXISTS track_search_key ON track_search(search_key,track_id);
CREATE INDEX IF NOT EXISTS track_search_bpm ON track_search(search_bpm,track_id);
CREATE INDEX IF NOT EXISTS tracks_year ON tracks(year) WHERE missing=0;
CREATE INDEX IF NOT EXISTS playback_sessions_track ON playback_sessions(track_id);
