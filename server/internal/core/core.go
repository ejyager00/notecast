// Package core is the application service: every state change to notes,
// pages and jobs happens here, each inside one transaction, with the note
// status machine (package notes) deciding what is legal.
package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/files"
	"git.ericbits.win/eric/notecast/server/internal/jobs"
	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/store"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict") // illegal in the note's current state
	ErrInvalid  = errors.New("invalid")  // bad input
)

const (
	DefaultLease    = 120 * time.Second
	MaxPageBytes    = 25 << 20
	MaxPagesPerNote = 40
)

type Service struct {
	db     *sql.DB
	files  *files.Store
	notify *jobs.Notifier
	now    func() time.Time
	Lease  time.Duration
}

func New(db *sql.DB, fs *files.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, files: fs, notify: jobs.NewNotifier(), now: now, Lease: DefaultLease}
}

func (s *Service) Files() *files.Store { return s.files }

// inTx runs fn in a transaction. If fn enqueued a job (set *enqueued), idle
// workers are woken after the commit, never before it.
func (s *Service) inTx(ctx context.Context, fn func(tx *sql.Tx, enqueued *bool) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	enqueued := false
	if err := fn(tx, &enqueued); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if enqueued {
		s.notify.Broadcast()
	}
	return nil
}

// --- reading ---------------------------------------------------------------

type Page struct {
	ID         int64
	Position   int
	Ext        string
	SHA256     string
	Bytes      int64
	HasDisplay bool
	Transcript *string
}

type Chapter struct {
	Start float64 `json:"start"`
	Title string  `json:"title"`
}

type Note struct {
	ID               int64
	PID              string
	CollectionID     *int64
	CollectionName   string
	Status           notes.Status
	FailedStage      notes.Kind
	Error            string
	Title            string
	Summary          string
	Tags             []string
	Narration        string
	NarrationStale   bool
	Voice            string // "" = default
	Speed            float64
	AudioVersion     int64
	AudioBytes       int64
	AudioSeconds     float64
	Chapters         []Chapter
	AudioPublishedAt time.Time
	Provenance       map[string]any
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Pages            []Page
	Job              *jobs.Job // active job, if any
}

// Previews counts the pages that have a JPEG copy for display.
func (n *Note) Previews() int {
	c := 0
	for _, p := range n.Pages {
		if p.HasDisplay {
			c++
		}
	}
	return c
}

// Transcript is the note's pages joined in order, as the narrator sees it.
func (n *Note) Transcript() string {
	out := ""
	for i, p := range n.Pages {
		if p.Transcript == nil {
			continue
		}
		if out != "" {
			out += "\n\n"
		}
		out += fmt.Sprintf("<!-- page %d -->\n%s", i+1, *p.Transcript)
	}
	return out
}

func (s *Service) Get(ctx context.Context, pid string) (*Note, error) {
	var n *Note
	err := s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		var err error
		n, err = loadNote(ctx, tx, pid)
		return err
	})
	return n, err
}

func loadNote(ctx context.Context, tx *sql.Tx, pid string) (*Note, error) {
	var n Note
	var coll sql.NullInt64
	var collName, failed, errMsg, voice, chapters, published sql.NullString
	var speed, seconds sql.NullFloat64
	var version, bytes sql.NullInt64
	var tags, prov, created, updated string
	var stale int
	err := tx.QueryRowContext(ctx, `SELECT n.id, n.pid, n.collection_id, c.name, n.status, n.failed_stage,
		n.error, n.title, n.summary, n.tags, n.narration, n.narration_stale, n.voice, n.speed,
		n.audio_version, n.audio_bytes, n.audio_seconds, n.audio_chapters, n.audio_published_at,
		n.provenance, n.created_at, n.updated_at
		FROM notes n LEFT JOIN collections c ON c.id = n.collection_id WHERE n.pid = ?`, pid).Scan(
		&n.ID, &n.PID, &coll, &collName, &n.Status, &failed, &errMsg, &n.Title, &n.Summary, &tags,
		&n.Narration, &stale, &voice, &speed, &version, &bytes, &seconds, &chapters, &published,
		&prov, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if coll.Valid {
		n.CollectionID = &coll.Int64
	}
	n.CollectionName = collName.String
	n.FailedStage = notes.Kind(failed.String)
	n.Error = errMsg.String
	n.NarrationStale = stale != 0
	n.Voice, n.Speed = voice.String, speed.Float64
	n.AudioVersion, n.AudioBytes, n.AudioSeconds = version.Int64, bytes.Int64, seconds.Float64
	if chapters.Valid {
		json.Unmarshal([]byte(chapters.String), &n.Chapters)
	}
	if published.Valid {
		n.AudioPublishedAt, _ = store.ParseTime(published.String)
	}
	json.Unmarshal([]byte(tags), &n.Tags)
	json.Unmarshal([]byte(prov), &n.Provenance)
	n.CreatedAt, _ = store.ParseTime(created)
	n.UpdatedAt, _ = store.ParseTime(updated)

	rows, err := tx.QueryContext(ctx, `SELECT id, position, orig_ext, orig_sha256, orig_bytes, has_display, transcript
		FROM pages WHERE note_id = ? ORDER BY position`, n.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Page
		var hasDisplay int
		var tr sql.NullString
		if err := rows.Scan(&p.ID, &p.Position, &p.Ext, &p.SHA256, &p.Bytes, &hasDisplay, &tr); err != nil {
			return nil, err
		}
		p.HasDisplay = hasDisplay != 0
		if tr.Valid {
			p.Transcript = &tr.String
		}
		n.Pages = append(n.Pages, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	n.Job, err = jobs.Active(ctx, tx, n.ID)
	return &n, err
}

// --- note lifecycle ----------------------------------------------------------

func (s *Service) CreateNote(ctx context.Context, collectionID *int64) (string, error) {
	pid := newPID()
	err := s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		if collectionID != nil {
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM collections WHERE id = ?`, *collectionID).Scan(new(int)); err != nil {
				return fmt.Errorf("%w: no such collection", ErrInvalid)
			}
		}
		ts := store.Now(s.now())
		_, err := tx.ExecContext(ctx, `INSERT INTO notes (pid, collection_id, status, created_at, updated_at)
			VALUES (?, ?, 'uploading', ?, ?)`, pid, collectionID, ts, ts)
		if err != nil {
			return err
		}
		return upsertFTS(ctx, tx, pid)
	})
	return pid, err
}

// transition applies ev to n and persists the result, enqueueing a job when
// the transition calls for one. n.Status is updated in place.
func (s *Service) transition(ctx context.Context, tx *sql.Tx, n *Note, ev notes.Event, errMsg string, enqueued *bool) error {
	r, err := notes.Apply(n.Status, ev, n.FailedStage)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	failed := any(nil)
	if r.Next == notes.Failed {
		failed = string(notes.KindFor(n.Status))
	}
	if r.Next != notes.Failed {
		errMsg = ""
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET status = ?, failed_stage = ?, error = ?, updated_at = ? WHERE id = ?`,
		r.Next, failed, nullable(errMsg), store.Now(s.now()), n.ID); err != nil {
		return err
	}
	n.Status = r.Next
	if r.Enqueue != "" {
		params := "{}"
		if r.Enqueue == notes.Transcribe {
			ids := untranscribed(n)
			if len(ids) == 0 {
				return fmt.Errorf("%w: no pages to transcribe", ErrConflict)
			}
			b, _ := json.Marshal(map[string]any{"page_ids": ids})
			params = string(b)
		}
		if _, err := jobs.Enqueue(ctx, tx, n.ID, r.Enqueue, params, s.now()); err != nil {
			if errors.Is(err, jobs.ErrActive) {
				return fmt.Errorf("%w: %v", ErrConflict, err)
			}
			return err
		}
		*enqueued = true
	}
	return nil
}

func untranscribed(n *Note) []int64 {
	ids := []int64{}
	for _, p := range n.Pages {
		if p.Transcript == nil {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// event loads the note and applies one user-driven event.
func (s *Service) event(ctx context.Context, pid string, ev notes.Event, check func(*Note) error) error {
	return s.inTx(ctx, func(tx *sql.Tx, enq *bool) error {
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(n); err != nil {
				return err
			}
		}
		return s.transition(ctx, tx, n, ev, "", enq)
	})
}

func (s *Service) Submit(ctx context.Context, pid string) error {
	return s.event(ctx, pid, notes.Submit, func(n *Note) error {
		if len(n.Pages) == 0 {
			return fmt.Errorf("%w: add at least one page first", ErrConflict)
		}
		return nil
	})
}

// TranscribeNew transcribes pages added to a note after its first pass.
func (s *Service) TranscribeNew(ctx context.Context, pid string) error {
	return s.event(ctx, pid, notes.TranscribeNew, nil)
}

func (s *Service) Rewrite(ctx context.Context, pid string) error {
	return s.event(ctx, pid, notes.Rewrite, func(n *Note) error {
		if n.Transcript() == "" {
			return fmt.Errorf("%w: nothing transcribed yet", ErrConflict)
		}
		return nil
	})
}

// Approve queues audio generation. A stale narration (transcript edited
// since it was written) is refused unless force is set.
func (s *Service) Approve(ctx context.Context, pid string, force bool) error {
	return s.event(ctx, pid, notes.Approve, func(n *Note) error {
		if n.Narration == "" {
			return fmt.Errorf("%w: narration is empty", ErrConflict)
		}
		if n.NarrationStale && !force {
			return fmt.Errorf("%w: the transcript changed since the narration was written", ErrConflict)
		}
		return nil
	})
}

func (s *Service) Retry(ctx context.Context, pid string) error {
	return s.event(ctx, pid, notes.Retry, nil)
}

func (s *Service) Cancel(ctx context.Context, pid string) error {
	return s.inTx(ctx, func(tx *sql.Tx, enq *bool) error {
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if n.Job == nil {
			return fmt.Errorf("%w: nothing to cancel", ErrConflict)
		}
		if err := jobs.Finish(ctx, tx, n.Job.ID, jobs.Cancelled, "cancelled", s.now()); err != nil {
			return err
		}
		return s.transition(ctx, tx, n, notes.Cancel, "", enq)
	})
}

func (s *Service) Delete(ctx context.Context, pid string) error {
	err := s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM notes_fts WHERE rowid = ?`, n.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, n.ID) // cascades pages, jobs
		return err
	})
	if err != nil {
		return err
	}
	return s.files.RemoveNote(pid)
}

// Edits are user changes from the review screen. Nil fields are unchanged.
type Edits struct {
	Title        *string
	CollectionID **int64 // set to a nil *int64 to clear
	Transcripts  map[int64]string
	Narration    *string
	Voice        *string
	Speed        *float64
}

func (s *Service) SaveEdits(ctx context.Context, pid string, e Edits) error {
	return s.inTx(ctx, func(tx *sql.Tx, enq *bool) error {
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if n.Job != nil {
			return fmt.Errorf("%w: wait for the current job to finish or cancel it", ErrConflict)
		}
		now := store.Now(s.now())
		contentChanged := false
		if e.Title != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET title = ?, updated_at = ? WHERE id = ?`, *e.Title, now, n.ID); err != nil {
				return err
			}
		}
		if e.CollectionID != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET collection_id = ?, updated_at = ? WHERE id = ?`, *e.CollectionID, now, n.ID); err != nil {
				return fmt.Errorf("%w: no such collection", ErrInvalid)
			}
		}
		if e.Voice != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET voice = ? WHERE id = ?`, nullable(*e.Voice), n.ID); err != nil {
				return err
			}
		}
		if e.Speed != nil {
			var speed any // 0 clears the override
			if *e.Speed != 0 {
				if *e.Speed < 0.5 || *e.Speed > 2 {
					return fmt.Errorf("%w: speed must be between 0.5 and 2", ErrInvalid)
				}
				speed = *e.Speed
			}
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET speed = ? WHERE id = ?`, speed, n.ID); err != nil {
				return err
			}
		}
		transcriptChanged := false
		for pageID, text := range e.Transcripts {
			var cur sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT transcript FROM pages WHERE id = ? AND note_id = ?`, pageID, n.ID).Scan(&cur); err != nil {
				return fmt.Errorf("%w: page %d is not part of this note", ErrInvalid, pageID)
			}
			if cur.Valid && cur.String == text {
				continue
			}
			if !cur.Valid {
				return fmt.Errorf("%w: page %d hasn't been transcribed yet", ErrConflict, pageID)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE pages SET transcript = ? WHERE id = ?`, text, pageID); err != nil {
				return err
			}
			transcriptChanged = true
		}
		if transcriptChanged {
			contentChanged = true
			if n.Narration != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE notes SET narration_stale = 1 WHERE id = ?`, n.ID); err != nil {
					return err
				}
			}
		}
		if e.Narration != nil && *e.Narration != n.Narration {
			contentChanged = true
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET narration = ? WHERE id = ?`, *e.Narration, n.ID); err != nil {
				return err
			}
		}
		// Content edits on a ready note send it back to review (its audio stays
		// live until replaced). Title, collection, voice and speed don't.
		if contentChanged && n.Status != notes.Review {
			if err := s.transition(ctx, tx, n, notes.Edit, "", enq); err != nil {
				return err
			}
		}
		return upsertFTS(ctx, tx, pid)
	})
}

// --- pages -------------------------------------------------------------------

func pagesEditable(n *Note) error {
	if n.Job != nil {
		return fmt.Errorf("%w: wait for the current job to finish or cancel it", ErrConflict)
	}
	switch n.Status {
	case notes.Uploading, notes.Review, notes.Ready:
		return nil
	}
	return fmt.Errorf("%w: pages can't change while the note is %s", ErrConflict, n.Status)
}

// AddPage stores one uploaded image (streamed from r) as the note's last page.
func (s *Service) AddPage(ctx context.Context, pid string, r io.Reader) (int64, error) {
	head := make([]byte, 16)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, fmt.Errorf("%w: empty upload", ErrInvalid)
	}
	head = head[:n]
	ext := files.SniffImage(head)
	if ext == "" {
		return 0, fmt.Errorf("%w: not a JPEG, PNG, WebP or HEIC image", ErrInvalid)
	}
	tmp, err := s.files.NewTemp()
	if err != nil {
		return 0, err
	}
	defer tmp.Discard()
	tmp.Write(head)
	if _, err := io.Copy(tmp, io.LimitReader(r, MaxPageBytes-int64(len(head))+1)); err != nil {
		return 0, err
	}
	if tmp.Size() > MaxPageBytes {
		return 0, fmt.Errorf("%w: image larger than %d MB", ErrInvalid, MaxPageBytes>>20)
	}
	var pageID int64
	err = s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		note, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if err := pagesEditable(note); err != nil {
			return err
		}
		if len(note.Pages) >= MaxPagesPerNote {
			return fmt.Errorf("%w: at most %d pages per note", ErrInvalid, MaxPagesPerNote)
		}
		pos := 1
		if len(note.Pages) > 0 {
			pos = note.Pages[len(note.Pages)-1].Position + 1
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO pages (note_id, position, orig_ext, orig_sha256, orig_bytes)
			VALUES (?, ?, ?, ?, ?)`, note.ID, pos, ext, tmp.SHA256(), tmp.Size())
		if err != nil {
			return err
		}
		if pageID, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET updated_at = ? WHERE id = ?`, store.Now(s.now()), note.ID); err != nil {
			return err
		}
		return tmp.Commit(s.files.PageOrig(pid, pageID, ext))
	})
	return pageID, err
}

func (s *Service) DeletePage(ctx context.Context, pid string, pageID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if err := pagesEditable(n); err != nil {
			return err
		}
		var transcribed bool
		if err := tx.QueryRowContext(ctx, `SELECT transcript IS NOT NULL FROM pages WHERE id = ? AND note_id = ?`,
			pageID, n.ID).Scan(&transcribed); err != nil {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pages WHERE id = ?`, pageID); err != nil {
			return err
		}
		if transcribed && n.Narration != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET narration_stale = 1 WHERE id = ?`, n.ID); err != nil {
				return err
			}
		}
		return upsertFTS(ctx, tx, pid)
	})
}

// MovePage swaps a page with its neighbour (delta -1 = earlier, +1 = later).
func (s *Service) MovePage(ctx context.Context, pid string, pageID int64, delta int) error {
	if delta != -1 && delta != 1 {
		return fmt.Errorf("%w: delta must be -1 or 1", ErrInvalid)
	}
	return s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		if err := pagesEditable(n); err != nil {
			return err
		}
		i := slices.IndexFunc(n.Pages, func(p Page) bool { return p.ID == pageID })
		if i < 0 {
			return ErrNotFound
		}
		j := i + delta
		if j < 0 || j >= len(n.Pages) {
			return nil // already at the end; nothing to do
		}
		a, b := n.Pages[i], n.Pages[j]
		// Three steps because (note_id, position) is UNIQUE.
		for _, st := range []struct {
			id  int64
			pos int
		}{{a.ID, -1}, {b.ID, a.Position}, {a.ID, b.Position}} {
			if _, err := tx.ExecContext(ctx, `UPDATE pages SET position = ? WHERE id = ?`, st.pos, st.id); err != nil {
				return err
			}
		}
		if a.Transcript != nil && b.Transcript != nil && n.Narration != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET narration_stale = 1 WHERE id = ?`, n.ID); err != nil {
				return err
			}
		}
		return upsertFTS(ctx, tx, pid)
	})
}

// --- collections and settings -----------------------------------------------

type Collection struct {
	ID    int64
	Name  string
	Notes int
}

func (s *Service) Collections(ctx context.Context) ([]Collection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.name, COUNT(n.id) FROM collections c
		LEFT JOIN notes n ON n.collection_id = c.id GROUP BY c.id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Name, &c.Notes); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) CreateCollection(ctx context.Context, name string) (int64, error) {
	if name == "" || len(name) > 80 {
		return 0, fmt.Errorf("%w: collection name must be 1-80 characters", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO collections (name, created_at) VALUES (?, ?)`, name, store.Now(s.now()))
	if err != nil {
		return 0, fmt.Errorf("%w: a collection with that name already exists", ErrConflict)
	}
	return res.LastInsertId()
}

func (s *Service) RenameCollection(ctx context.Context, id int64, name string) error {
	if name == "" || len(name) > 80 {
		return fmt.Errorf("%w: collection name must be 1-80 characters", ErrInvalid)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE collections SET name = ? WHERE id = ?`, name, id)
	return err
}

func (s *Service) DeleteCollection(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, id) // notes keep, collection_id -> NULL
	return err
}

// Settings are a small typed view over the settings table.
type Settings struct {
	AutoApprove           bool
	DefaultVoice          string
	DefaultSpeed          float64
	IllegiblePolicy       string // "skip" | "say"
	FeedIncludeTranscript bool
	FeedToken             string // read-only here; see RotateFeedToken
}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var st Settings
	err := s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		var err error
		st, err = loadSettings(ctx, tx)
		return err
	})
	return st, err
}

func loadSettings(ctx context.Context, tx *sql.Tx) (Settings, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	st := Settings{DefaultVoice: "af_heart", DefaultSpeed: 1, IllegiblePolicy: "skip"}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return st, err
		}
		switch k {
		case "auto_approve":
			st.AutoApprove = v == "true"
		case "default_voice":
			st.DefaultVoice = v
		case "default_speed":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				st.DefaultSpeed = f
			}
		case "illegible_policy":
			st.IllegiblePolicy = v
		case "feed_include_transcript":
			st.FeedIncludeTranscript = v == "true"
		case "feed_token":
			st.FeedToken = v
		}
	}
	return st, rows.Err()
}

func (s *Service) SaveSettings(ctx context.Context, st Settings) error {
	if st.IllegiblePolicy != "skip" && st.IllegiblePolicy != "say" {
		return fmt.Errorf("%w: illegible policy must be skip or say", ErrInvalid)
	}
	if st.DefaultSpeed < 0.5 || st.DefaultSpeed > 2 {
		return fmt.Errorf("%w: speed must be between 0.5 and 2", ErrInvalid)
	}
	return s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		for k, v := range map[string]string{
			"auto_approve":            strconv.FormatBool(st.AutoApprove),
			"default_voice":           st.DefaultVoice,
			"default_speed":           strconv.FormatFloat(st.DefaultSpeed, 'f', -1, 64),
			"illegible_policy":        st.IllegiblePolicy,
			"feed_include_transcript": strconv.FormatBool(st.FeedIncludeTranscript),
		} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
				ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingJobs counts queued and running jobs (for "N notes waiting").
func (s *Service) PendingJobs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state IN ('queued','leased')`).Scan(&n)
	return n, err
}

// --- helpers -------------------------------------------------------------------

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const pidAlphabet = "abcdefghijkmnpqrstuvwxyz23456789" // 32 symbols, no 0/o/1/l

func newPID() string {
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = pidAlphabet[int(b[i])%len(pidAlphabet)]
	}
	return string(b)
}
