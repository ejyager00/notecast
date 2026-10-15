-- Narration is written, stored and edited per page (ARCHITECTURE §3.3), so a
-- rewrite redoes only the pages whose transcript changed and keeps the rest,
-- hand edits included. notes.narration stays as the pages' narrations joined
-- in order (core.syncNarration), which is what speech, search and the feed
-- read. Notes narrated before this have no page narrations; they keep their
-- single narration until rewritten.
ALTER TABLE pages ADD COLUMN narration TEXT;               -- NULL = never narrated
ALTER TABLE pages ADD COLUMN narration_stale INTEGER NOT NULL DEFAULT 0;  -- transcript changed since
ALTER TABLE pages ADD COLUMN narration_edited INTEGER NOT NULL DEFAULT 0; -- typed by her, not the model
