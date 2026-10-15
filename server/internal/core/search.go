package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/search"
	"git.ericbits.win/eric/notecast/server/internal/store"
)

// upsertFTS rewrites a note's search row from its current content. Called in
// the same transaction as every write that changes searchable text.
func upsertFTS(ctx context.Context, tx *sql.Tx, pid string) error {
	n, err := loadNote(ctx, tx, pid)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM notes_fts WHERE rowid = ?`, n.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notes_fts (rowid, title, tags, summary, transcript, narration)
		VALUES (?, ?, ?, ?, ?, ?)`, n.ID, n.Title, strings.Join(n.Tags, " "), n.Summary, n.Transcript(), n.Narration)
	return err
}

type ListItem struct {
	PID            string
	Title          string
	Summary        string
	Tags           []string
	Status         notes.Status
	CollectionName string
	AudioSeconds   float64
	Pages          int
	CreatedAt      time.Time
	Snippet        string // search only; highlights wrapped in search.MarkStart/MarkEnd
}

// List returns notes newest first, optionally in one collection.
func (s *Service) List(ctx context.Context, collectionID *int64) ([]ListItem, error) {
	q := listSelect + ` WHERE (? IS NULL OR n.collection_id = ?) ORDER BY n.created_at DESC LIMIT 500`
	return s.list(ctx, q, "", collectionID, collectionID)
}

// Search ranks notes by relevance: title matches count most, then tags,
// summary, and the body text.
func (s *Service) Search(ctx context.Context, input string, collectionID *int64) ([]ListItem, error) {
	fq := search.Query(input)
	if fq == "" {
		return s.List(ctx, collectionID)
	}
	q := `SELECT n.pid, n.title, n.summary, n.tags, n.status, COALESCE(c.name, ''),
		COALESCE(n.audio_seconds, 0), (SELECT COUNT(*) FROM pages p WHERE p.note_id = n.id), n.created_at,
		snippet(notes_fts, -1, '` + search.MarkStart + `', '` + search.MarkEnd + `', '…', 14)
		FROM notes_fts JOIN notes n ON n.id = notes_fts.rowid
		LEFT JOIN collections c ON c.id = n.collection_id
		WHERE notes_fts MATCH ? AND (? IS NULL OR n.collection_id = ?)
		ORDER BY bm25(notes_fts, 10.0, 5.0, 3.0, 1.0, 1.0) LIMIT 100`
	return s.list(ctx, q, "snippet", fq, collectionID, collectionID)
}

const listSelect = `SELECT n.pid, n.title, n.summary, n.tags, n.status, COALESCE(c.name, ''),
	COALESCE(n.audio_seconds, 0), (SELECT COUNT(*) FROM pages p WHERE p.note_id = n.id), n.created_at
	FROM notes n LEFT JOIN collections c ON c.id = n.collection_id`

func (s *Service) list(ctx context.Context, q, mode string, args ...any) ([]ListItem, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListItem
	for rows.Next() {
		var it ListItem
		var tags, created string
		dest := []any{&it.PID, &it.Title, &it.Summary, &tags, &it.Status, &it.CollectionName,
			&it.AudioSeconds, &it.Pages, &created}
		if mode == "snippet" {
			dest = append(dest, &it.Snippet)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(tags), &it.Tags)
		it.CreatedAt, _ = store.ParseTime(created)
		out = append(out, it)
	}
	return out, rows.Err()
}
