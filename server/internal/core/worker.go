package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/files"
	"git.ericbits.win/eric/notecast/server/internal/jobs"
	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/store"
)

// ErrLeaseLost is returned to a worker whose lease is gone; it must abandon
// the job. (Re-exported so HTTP code needn't import jobs.)
var ErrLeaseLost = jobs.ErrLeaseLost

// WorkerInfo is what a worker reports about itself on every call.
type WorkerInfo struct {
	ID      string `json:"worker_id"`
	Version string `json:"version"`
	State   string `json:"state"`  // idle | busy | gpu_unavailable | ...
	Detail  string `json:"detail"` // free text for the status banner
}

type Worker struct {
	WorkerInfo
	LastSeen time.Time
}

func seen(ctx context.Context, tx *sql.Tx, w WorkerInfo, now time.Time) error {
	if w.ID == "" {
		return fmt.Errorf("%w: worker_id is required", ErrInvalid)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO workers (id, version, last_seen_at, state, detail) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET version = excluded.version, last_seen_at = excluded.last_seen_at,
		state = excluded.state, detail = excluded.detail`, w.ID, w.Version, store.Now(now), w.State, w.Detail)
	return err
}

func (s *Service) Workers(ctx context.Context) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, version, last_seen_at, state, detail FROM workers ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worker
	for rows.Next() {
		var w Worker
		var ts string
		if err := rows.Scan(&w.ID, &w.Version, &ts, &w.State, &w.Detail); err != nil {
			return nil, err
		}
		w.LastSeen, _ = store.ParseTime(ts)
		out = append(out, w)
	}
	return out, rows.Err()
}

// --- claim -------------------------------------------------------------------

type ClaimPage struct {
	PageID     int64   `json:"page_id"`
	Position   int     `json:"position"`
	SHA256     string  `json:"sha256,omitempty"`
	Input      string  `json:"input,omitempty"`      // path under /worker/v1/jobs/{id}/inputs/, transcribe only
	Transcript *string `json:"transcript,omitempty"` // existing text of pages not being transcribed
	Narration  *string `json:"narration,omitempty"`  // the page's current narration, kept unless Narrate
	Narrate    bool    `json:"narrate,omitempty"`    // write this page's narration
}

// Claimed is everything a worker needs to run one job.
type Claimed struct {
	JobID          int64       `json:"job_id"`
	Kind           notes.Kind  `json:"kind"`
	LeaseToken     string      `json:"lease_token"`
	LeaseExpiresAt time.Time   `json:"lease_expires_at"`
	Attempt        int         `json:"attempt"`
	NotePID        string      `json:"note_pid"`
	Title          string      `json:"title"`
	Collection     string      `json:"collection,omitempty"`
	Pages          []ClaimPage `json:"pages,omitempty"`      // transcribe, narrate
	Transcribe     []int64     `json:"transcribe,omitempty"` // transcribe: page ids to OCR
	Meta           bool        `json:"meta,omitempty"`       // transcribe, narrate: also title, summary, tags
	Narration      string      `json:"narration,omitempty"`  // speak
	Voice          string      `json:"voice,omitempty"`      // speak
	Speed          float64     `json:"speed,omitempty"`      // speak
	Illegible      string      `json:"illegible_policy,omitempty"`
}

type ClaimRequest struct {
	WorkerInfo
	Kinds []notes.Kind `json:"kinds"`
}

// Claim leases the next job for the worker, waiting up to wait for one to
// appear. Returns nil, nil if none did.
func (s *Service) Claim(ctx context.Context, req ClaimRequest, wait time.Duration) (*Claimed, error) {
	for _, k := range req.Kinds {
		if k != notes.Transcribe && k != notes.Narrate && k != notes.Speak {
			return nil, fmt.Errorf("%w: unknown job kind %q", ErrInvalid, k)
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woken := s.notify.C() // before checking, so an enqueue in between still wakes us
		c, err := s.claimOnce(ctx, req)
		if c != nil || err != nil {
			return c, err
		}
		select {
		case <-woken:
		case <-time.After(5 * time.Second): // a retry backoff may have expired
		case <-deadline.C:
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}
}

func (s *Service) claimOnce(ctx context.Context, req ClaimRequest) (*Claimed, error) {
	var c *Claimed
	err := s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		now := s.now()
		if err := seen(ctx, tx, req.WorkerInfo, now); err != nil {
			return err
		}
		j, err := jobs.ClaimNext(ctx, tx, req.Kinds, req.ID, now, s.Lease)
		if err != nil || j == nil {
			return err
		}
		c, err = s.payload(ctx, tx, j)
		return err
	})
	return c, err
}

func (s *Service) payload(ctx context.Context, tx *sql.Tx, j *jobs.Job) (*Claimed, error) {
	var pid string
	if err := tx.QueryRowContext(ctx, `SELECT pid FROM notes WHERE id = ?`, j.NoteID).Scan(&pid); err != nil {
		return nil, err
	}
	n, err := loadNote(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	st, err := loadSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	c := &Claimed{
		JobID: j.ID, Kind: j.Kind, LeaseToken: j.LeaseToken, LeaseExpiresAt: j.LeaseUntil,
		Attempt: j.Attempt, NotePID: n.PID, Title: n.Title, Collection: n.CollectionName,
		Illegible: st.IllegiblePolicy,
	}
	switch j.Kind {
	case notes.Transcribe, notes.Narrate:
		ocr, err := transcribeIDs(j)
		if err != nil {
			return nil, err
		}
		c.Transcribe = ocr
		todo := narrateIDs(n, ocr)
		c.Meta = wantMeta(n, ocr, todo)
		for _, pg := range n.Pages {
			cp := ClaimPage{PageID: pg.ID, Position: pg.Position, Narration: pg.Narration,
				Narrate: slices.Contains(todo, pg.ID)}
			if slices.Contains(ocr, pg.ID) {
				cp.SHA256 = pg.SHA256
				cp.Input = fmt.Sprintf("page-%d", pg.ID)
			} else {
				cp.Transcript = pg.Transcript
			}
			c.Pages = append(c.Pages, cp)
		}
	case notes.Speak:
		c.Narration = n.Narration
		c.Voice, c.Speed = n.Voice, n.Speed
		if c.Voice == "" {
			c.Voice = st.DefaultVoice
		}
		if c.Speed == 0 {
			c.Speed = st.DefaultSpeed
		}
	}
	return c, nil
}

// transcribeIDs are the pages a transcribe job OCRs (none for other kinds).
func transcribeIDs(j *jobs.Job) ([]int64, error) {
	if j.Kind != notes.Transcribe {
		return nil, nil
	}
	var p struct {
		PageIDs []int64 `json:"page_ids"`
	}
	err := json.Unmarshal([]byte(j.Params), &p)
	return p.PageIDs, err
}

// narrateIDs are the pages a job writes narration for: those being read now
// and those that need it (Page.NeedsNarration). Claim and Complete both
// compute it, from the same state: nothing can edit a note while it has a job.
func narrateIDs(n *Note, ocr []int64) []int64 {
	ids := []int64{}
	for _, p := range n.Pages {
		if slices.Contains(ocr, p.ID) || p.NeedsNarration() {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// wantMeta: regenerate the title, summary and tags when the note is new,
// gained pages, or is being narrated from scratch (every page either redone
// or hers). A fix to some pages keeps them.
func wantMeta(n *Note, ocr, todo []int64) bool {
	if n.Summary == "" || len(ocr) > 0 {
		return true
	}
	for _, p := range n.Pages {
		if p.Transcript == nil {
			continue
		}
		if p.Narration == nil {
			return true
		}
		if !p.NarrationEdited && !slices.Contains(todo, p.ID) {
			return false // a model-written page kept as it is
		}
	}
	return true
}

// Input returns the stored original for one page of a leased transcribe job.
func (s *Service) Input(ctx context.Context, jobID int64, token, name string) (path, contentType string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		j, err := jobs.CheckLease(ctx, tx, jobID, token, s.now())
		if err != nil {
			return err
		}
		pageStr, ok := strings.CutPrefix(name, "page-")
		if !ok || j.Kind != notes.Transcribe {
			return ErrNotFound
		}
		id, err := strconv.ParseInt(pageStr, 10, 64)
		if err != nil {
			return ErrNotFound
		}
		var pid, ext string
		if err := tx.QueryRowContext(ctx, `SELECT n.pid, p.orig_ext FROM pages p JOIN notes n ON n.id = p.note_id
			WHERE p.id = ? AND p.note_id = ?`, id, j.NoteID).Scan(&pid, &ext); err != nil {
			return ErrNotFound
		}
		path, contentType = s.files.PageOrig(pid, id, ext), files.ContentType(ext)
		return nil
	})
	return path, contentType, err
}

func (s *Service) Heartbeat(ctx context.Context, jobID int64, token, progress string, w WorkerInfo) error {
	return s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		now := s.now()
		if err := seen(ctx, tx, w, now); err != nil {
			return err
		}
		if _, err := jobs.CheckLease(ctx, tx, jobID, token, now); err != nil {
			return err
		}
		return jobs.Extend(ctx, tx, jobID, progress, now, s.Lease)
	})
}

// Previews stores display and thumb JPEGs for a transcribe job's pages before
// the job finishes, so the note shows its pages while OCR runs (originals are
// often HEIC, which most non-Safari browsers can't show). out holds
// display-<id>.jpg and thumb-<id>.jpg pairs; any subset of the job's pages.
func (s *Service) Previews(ctx context.Context, jobID int64, token string, out map[string][]byte, w WorkerInfo) error {
	return s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		now := s.now()
		if err := seen(ctx, tx, w, now); err != nil {
			return err
		}
		j, err := jobs.CheckLease(ctx, tx, jobID, token, now)
		if err != nil {
			return err
		}
		if j.Kind != notes.Transcribe {
			return fmt.Errorf("%w: previews are for transcribe jobs", ErrInvalid)
		}
		var p struct {
			PageIDs []int64 `json:"page_ids"`
		}
		if err := json.Unmarshal([]byte(j.Params), &p); err != nil {
			return err
		}
		var pid string
		if err := tx.QueryRowContext(ctx, `SELECT pid FROM notes WHERE id = ?`, j.NoteID).Scan(&pid); err != nil {
			return err
		}
		var got int
		for _, id := range p.PageIDs {
			display, thumb := out[fmt.Sprintf("display-%d.jpg", id)], out[fmt.Sprintf("thumb-%d.jpg", id)]
			if display == nil && thumb == nil {
				continue
			}
			if !files.IsJPEG(display) || !files.IsJPEG(thumb) {
				return fmt.Errorf("%w: page %d needs display and thumb JPEGs", ErrInvalid, id)
			}
			if err := s.files.WriteFile(s.files.PageDisplay(pid, id), display); err != nil {
				return err
			}
			if err := s.files.WriteFile(s.files.PageThumb(pid, id), thumb); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE pages SET has_display = 1 WHERE id = ? AND note_id = ?`, id, j.NoteID); err != nil {
				return err
			}
			got += 2
		}
		if got != len(out) {
			return fmt.Errorf("%w: previews for pages outside this job", ErrInvalid)
		}
		return nil
	})
}

// --- completion --------------------------------------------------------------

type PageResult struct {
	PageID     int64  `json:"page_id"`
	Transcript string `json:"transcript"`
}

// SegmentResult is one page's narration.
type SegmentResult struct {
	PageID    int64  `json:"page_id"`
	Narration string `json:"narration"`
}

type MetaResult struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Tags    []string `json:"tags"`
}

type AudioResult struct {
	Seconds  float64   `json:"seconds"`
	Chapters []Chapter `json:"chapters"`
	Voice    string    `json:"voice"`
	Speed    float64   `json:"speed"`
}

// Result is a worker's result.json. Which fields are required depends on
// the job kind; Complete validates.
type Result struct {
	Pages      []PageResult    `json:"pages,omitempty"`
	Segments   []SegmentResult `json:"segments,omitempty"` // transcribe, narrate
	Meta       *MetaResult     `json:"meta,omitempty"`     // when the claim asked for it
	Audio      *AudioResult    `json:"audio,omitempty"`
	Provenance map[string]any  `json:"provenance,omitempty"`
}

// Files a worker may upload with a result, and their limits.
const (
	MaxImageResult = 8 << 20
	MaxAudioResult = 200 << 20
)

// Complete applies a finished job. Output files are written only after the
// lease is verified, and the whole update is one transaction, so a stale
// completion changes nothing.
func (s *Service) Complete(ctx context.Context, jobID int64, token string, res Result, out map[string][]byte, w WorkerInfo) error {
	var oldAudio []string
	err := s.inTx(ctx, func(tx *sql.Tx, enq *bool) error {
		now := s.now()
		if err := seen(ctx, tx, w, now); err != nil {
			return err
		}
		j, err := jobs.CheckLease(ctx, tx, jobID, token, now)
		if err != nil {
			return err
		}
		// Done first: until it is, the job counts as the note's active one and
		// the unique index would refuse a follow-on job (auto-approve's speak).
		// Any error below rolls this back with everything else.
		if err := jobs.Finish(ctx, tx, j.ID, jobs.Done, "", now); err != nil {
			return err
		}
		var pid string
		if err := tx.QueryRowContext(ctx, `SELECT pid FROM notes WHERE id = ?`, j.NoteID).Scan(&pid); err != nil {
			return err
		}
		n, err := loadNote(ctx, tx, pid)
		if err != nil {
			return err
		}
		switch j.Kind {
		case notes.Transcribe:
			err = s.completeTranscribe(ctx, tx, n, j, res, out, enq)
		case notes.Narrate:
			err = s.completeNarrate(ctx, tx, n, j, res, enq)
		case notes.Speak:
			oldAudio, err = s.completeSpeak(ctx, tx, n, res, out, enq)
		}
		if err != nil {
			return err
		}
		if err := mergeProvenance(ctx, tx, n, j.Kind, res.Provenance); err != nil {
			return err
		}
		return upsertFTS(ctx, tx, pid)
	})
	if err == nil {
		for _, p := range oldAudio {
			os.Remove(p) // best effort; superseded audio version
		}
	}
	return err
}

func (s *Service) completeTranscribe(ctx context.Context, tx *sql.Tx, n *Note, j *jobs.Job, res Result, out map[string][]byte, enq *bool) error {
	var p struct {
		PageIDs []int64 `json:"page_ids"`
	}
	if err := json.Unmarshal([]byte(j.Params), &p); err != nil {
		return err
	}
	got := map[int64]string{}
	for _, pr := range res.Pages {
		got[pr.PageID] = pr.Transcript
	}
	if len(got) != len(p.PageIDs) {
		return fmt.Errorf("%w: expected transcripts for pages %v", ErrInvalid, p.PageIDs)
	}
	for _, id := range p.PageIDs {
		text, ok := got[id]
		if !ok {
			return fmt.Errorf("%w: missing transcript for page %d", ErrInvalid, id)
		}
		display, thumb := out[fmt.Sprintf("display-%d.jpg", id)], out[fmt.Sprintf("thumb-%d.jpg", id)]
		if !files.IsJPEG(display) || !files.IsJPEG(thumb) {
			return fmt.Errorf("%w: page %d needs display and thumb JPEGs", ErrInvalid, id)
		}
		if err := s.files.WriteFile(s.files.PageDisplay(n.PID, id), display); err != nil {
			return err
		}
		if err := s.files.WriteFile(s.files.PageThumb(n.PID, id), thumb); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pages SET transcript = ?, has_display = 1 WHERE id = ? AND note_id = ?`,
			text, id, n.ID); err != nil {
			return err
		}
	}
	if err := saveNarration(ctx, tx, n, j, res, s.now()); err != nil {
		return err
	}
	st, err := loadSettings(ctx, tx)
	if err != nil {
		return err
	}
	ev := notes.TranscribeDone
	if st.AutoApprove {
		ev = notes.TranscribeDoneAuto
	}
	return s.transition(ctx, tx, n, ev, "", enq)
}

func (s *Service) completeNarrate(ctx context.Context, tx *sql.Tx, n *Note, j *jobs.Job, res Result, enq *bool) error {
	if err := saveNarration(ctx, tx, n, j, res, s.now()); err != nil {
		return err
	}
	return s.transition(ctx, tx, n, notes.NarrateDone, "", enq)
}

func (s *Service) completeSpeak(ctx context.Context, tx *sql.Tx, n *Note, res Result, out map[string][]byte, enq *bool) ([]string, error) {
	a := res.Audio
	mp3 := out["audio.mp3"]
	if a == nil || a.Seconds <= 0 || !files.IsMP3(mp3) {
		return nil, fmt.Errorf("%w: speak result needs audio metadata and an MP3", ErrInvalid)
	}
	version := n.AudioVersion + 1
	if err := s.files.WriteFile(s.files.Audio(n.PID, version), mp3); err != nil {
		return nil, err
	}
	chapters, _ := json.Marshal(a.Chapters)
	if raw, ok := out["chapters.json"]; ok {
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%w: chapters.json is not JSON", ErrInvalid)
		}
		if err := s.files.WriteFile(s.files.Chapters(n.PID, version), raw); err != nil {
			return nil, err
		}
	}
	now := store.Now(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET audio_version = ?, audio_bytes = ?, audio_seconds = ?,
		audio_chapters = ?, audio_published_at = ?, updated_at = ? WHERE id = ?`,
		version, len(mp3), a.Seconds, string(chapters), now, now, n.ID); err != nil {
		return nil, err
	}
	var old []string
	if n.AudioVersion > 0 {
		old = []string{s.files.Audio(n.PID, n.AudioVersion), s.files.Chapters(n.PID, n.AudioVersion)}
	}
	return old, s.transition(ctx, tx, n, notes.SpeakDone, "", enq)
}

// saveNarration stores a job's page narrations, and its title, summary and
// tags if the claim asked for them. n is the note as it was when claimed.
func saveNarration(ctx context.Context, tx *sql.Tx, n *Note, j *jobs.Job, res Result, now time.Time) error {
	ocr, err := transcribeIDs(j)
	if err != nil {
		return err
	}
	todo := narrateIDs(n, ocr)
	got := map[int64]string{}
	for _, sr := range res.Segments {
		if !slices.Contains(todo, sr.PageID) {
			return fmt.Errorf("%w: narration for page %d, which wasn't asked for", ErrInvalid, sr.PageID)
		}
		got[sr.PageID] = strings.TrimSpace(sr.Narration)
	}
	for _, id := range todo {
		text, ok := got[id]
		if !ok {
			return fmt.Errorf("%w: missing narration for page %d", ErrInvalid, id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pages SET narration = ?, narration_stale = 0,
			narration_edited = 0 WHERE id = ? AND note_id = ?`, text, id, n.ID); err != nil {
			return err
		}
	}
	if err := syncNarration(ctx, tx, n.ID); err != nil {
		return err
	}
	var joined string
	if err := tx.QueryRowContext(ctx, `SELECT narration FROM notes WHERE id = ?`, n.ID).Scan(&joined); err != nil {
		return err
	}
	if strings.TrimSpace(joined) == "" {
		return fmt.Errorf("%w: narration is empty", ErrInvalid)
	}
	ts := store.Now(now)
	m := res.Meta
	if m == nil {
		if wantMeta(n, ocr, todo) {
			return fmt.Errorf("%w: result needs a title, summary and tags", ErrInvalid)
		}
		_, err := tx.ExecContext(ctx, `UPDATE notes SET updated_at = ? WHERE id = ?`, ts, n.ID)
		return err
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	tagJSON, _ := json.Marshal(tags)
	// A title the user typed wins over a generated one.
	title := n.Title
	if title == "" {
		title = m.Title
	}
	_, err = tx.ExecContext(ctx, `UPDATE notes SET title = ?, summary = ?, tags = ?, updated_at = ? WHERE id = ?`,
		title, m.Summary, string(tagJSON), ts, n.ID)
	return err
}

func mergeProvenance(ctx context.Context, tx *sql.Tx, n *Note, kind notes.Kind, p map[string]any) error {
	if len(p) == 0 {
		return nil
	}
	all := n.Provenance
	if all == nil {
		all = map[string]any{}
	}
	all[string(kind)] = p
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE notes SET provenance = ? WHERE id = ?`, string(b), n.ID)
	return err
}

// --- failure -------------------------------------------------------------------

// FailJob records a failed attempt. Retryable failures with attempts left
// go back in the queue after a backoff; otherwise the note becomes failed.
func (s *Service) FailJob(ctx context.Context, jobID int64, token, msg string, retryable bool, w WorkerInfo) error {
	return s.inTx(ctx, func(tx *sql.Tx, _ *bool) error {
		now := s.now()
		if err := seen(ctx, tx, w, now); err != nil {
			return err
		}
		j, err := jobs.CheckLease(ctx, tx, jobID, token, now)
		if err != nil {
			return err
		}
		return s.failAttempt(ctx, tx, j, msg, retryable)
	})
}

func (s *Service) failAttempt(ctx context.Context, tx *sql.Tx, j *jobs.Job, msg string, retryable bool) error {
	now := s.now()
	msg = truncate(msg, 2000)
	if retryable && j.Attempt < j.MaxAttempts {
		return jobs.Requeue(ctx, tx, j, msg, now)
	}
	if err := jobs.Finish(ctx, tx, j.ID, jobs.Failed, msg, now); err != nil {
		return err
	}
	var pid string
	if err := tx.QueryRowContext(ctx, `SELECT pid FROM notes WHERE id = ?`, j.NoteID).Scan(&pid); err != nil {
		return err
	}
	n, err := loadNote(ctx, tx, pid)
	if err != nil {
		return err
	}
	enq := false
	return s.transition(ctx, tx, n, notes.Fail, msg, &enq)
}

// Reap treats expired leases as retryable failures. Run it periodically.
func (s *Service) Reap(ctx context.Context) (int, error) {
	count := 0
	err := s.inTx(ctx, func(tx *sql.Tx, enq *bool) error {
		expired, err := jobs.Expired(ctx, tx, s.now())
		if err != nil {
			return err
		}
		for _, j := range expired {
			if err := s.failAttempt(ctx, tx, j, "worker stopped responding (lease expired)", true); err != nil {
				return err
			}
			count++
		}
		*enq = count > 0 // requeued jobs become claimable after their backoff
		return nil
	})
	return count, err
}

// RunReaper reaps every interval until ctx ends.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Reap(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logf("reaper: %v", err)
			} else if n > 0 {
				logf("reaper: requeued or failed %d expired job(s)", n)
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
