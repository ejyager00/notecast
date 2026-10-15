-- notecast schema v1. Forward-only: never edit this file once applied;
-- add 0002_*.sql instead. Times are RFC 3339 UTC text.

CREATE TABLE collections (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);

CREATE TABLE notes (
  id INTEGER PRIMARY KEY,
  pid TEXT NOT NULL UNIQUE,
  collection_id INTEGER REFERENCES collections(id) ON DELETE SET NULL,
  status TEXT NOT NULL CHECK (status IN
    ('uploading','transcribing','review','narrating','speaking','ready','failed')),
  failed_stage TEXT CHECK (failed_stage IN ('transcribe','narrate','speak')),
  error TEXT,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  tags TEXT NOT NULL DEFAULT '[]',
  narration TEXT NOT NULL DEFAULT '',
  narration_stale INTEGER NOT NULL DEFAULT 0,
  voice TEXT,
  speed REAL,
  audio_version INTEGER,
  audio_bytes INTEGER,
  audio_seconds REAL,
  audio_chapters TEXT,
  audio_published_at TEXT,
  provenance TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX notes_collection ON notes(collection_id, created_at);

CREATE TABLE pages (
  id INTEGER PRIMARY KEY,
  note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  orig_ext TEXT NOT NULL,
  orig_sha256 TEXT NOT NULL,
  orig_bytes INTEGER NOT NULL,
  has_display INTEGER NOT NULL DEFAULT 0,
  transcript TEXT,
  UNIQUE (note_id, position)
);

CREATE TABLE jobs (
  id INTEGER PRIMARY KEY,
  note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('transcribe','narrate','speak')),
  state TEXT NOT NULL CHECK (state IN ('queued','leased','done','failed','cancelled')),
  params TEXT NOT NULL DEFAULT '{}',
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  not_before TEXT,
  lease_token TEXT,
  lease_expires_at TEXT,
  worker_id TEXT,
  progress TEXT,
  error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
-- The invariant that makes editing-vs-worker races impossible.
CREATE UNIQUE INDEX jobs_one_active_per_note ON jobs(note_id)
  WHERE state IN ('queued','leased');
CREATE INDEX jobs_claimable ON jobs(state, not_before, id);

CREATE TABLE workers (
  id TEXT PRIMARY KEY,
  version TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings (key, value) VALUES
  ('auto_approve', 'false'),
  ('default_voice', 'af_heart'),
  ('default_speed', '1.0'),
  ('illegible_policy', 'skip'),
  ('feed_include_transcript', 'false');

-- rowid = notes.id; maintained by application code (core.upsertFTS) in the
-- same transaction as every note write.
CREATE VIRTUAL TABLE notes_fts USING fts5(
  title, tags, summary, transcript, narration,
  tokenize = 'porter unicode61'
);
