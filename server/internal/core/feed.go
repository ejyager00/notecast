package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"time"
)

// RotateFeedToken issues a new feed token, invalidating every old feed URL.
//
// The token is stored as-is rather than hashed: anyone who can read the
// database can read every note anyway, and keeping it lets Settings show the
// feed URLs again whenever they're needed.
func (s *Service) RotateFeedToken(ctx context.Context) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('feed_token', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, tok)
	return tok, err
}

// FeedTokenValid reports whether tok is the current feed token.
func (s *Service) FeedTokenValid(ctx context.Context, tok string) bool {
	var cur string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'feed_token'`).Scan(&cur); err != nil {
		return false
	}
	return cur != "" && subtle.ConstantTimeCompare([]byte(cur), []byte(tok)) == 1
}

type FeedItem struct {
	PID            string
	Title          string
	Summary        string
	Tags           []string
	Transcript     string // only filled when the feed includes transcripts
	CollectionName string
	AudioVersion   int64
	AudioBytes     int64
	AudioSeconds   float64
	HasChapters    bool
	Published      time.Time
}

// FeedItems lists notes that have audio, newest audio first. A note under
// re-review keeps its previous audio in the feed until new audio replaces it.
func (s *Service) FeedItems(ctx context.Context, collectionID *int64, withTranscript bool) ([]FeedItem, error) {
	var out []FeedItem
	err := s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		rows, err := tx.QueryContext(ctx, `SELECT n.pid FROM notes n
			WHERE n.audio_version IS NOT NULL AND (? IS NULL OR n.collection_id = ?)
			ORDER BY n.audio_published_at DESC, n.id DESC LIMIT 300`, collectionID, collectionID)
		if err != nil {
			return err
		}
		var pids []string
		for rows.Next() {
			var pid string
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return err
			}
			pids = append(pids, pid)
		}
		rows.Close()
		for _, pid := range pids {
			n, err := loadNote(ctx, tx, pid)
			if err != nil {
				return err
			}
			it := FeedItem{
				PID: n.PID, Title: n.Title, Summary: n.Summary, Tags: n.Tags,
				CollectionName: n.CollectionName, AudioVersion: n.AudioVersion,
				AudioBytes: n.AudioBytes, AudioSeconds: n.AudioSeconds,
				HasChapters: len(n.Chapters) > 1, Published: n.AudioPublishedAt,
			}
			if withTranscript {
				it.Transcript = n.Transcript()
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// CollectionName returns a collection's name, or ErrNotFound.
func (s *Service) CollectionName(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM collections WHERE id = ?`, id).Scan(&name)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return name, err
}

// ChaptersJSON returns Podcasting 2.0 chapters for a note's current audio.
func (s *Service) ChaptersJSON(ctx context.Context, pid string) ([]byte, error) {
	n, err := s.Get(ctx, pid)
	if err != nil {
		return nil, err
	}
	type ch struct {
		StartTime float64 `json:"startTime"`
		Title     string  `json:"title"`
	}
	doc := struct {
		Version  string `json:"version"`
		Chapters []ch   `json:"chapters"`
	}{Version: "1.2.0"}
	for _, c := range n.Chapters {
		doc.Chapters = append(doc.Chapters, ch{c.Start, c.Title})
	}
	return json.Marshal(doc)
}
