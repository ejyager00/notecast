// Package jobs is the job table's mechanics: enqueue, claim with a lease,
// lease validation, and the in-process notifier that wakes long-polling
// workers. Status changes on notes are the caller's business (package core);
// every function here runs inside the caller's transaction.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/store"
)

type State string

const (
	Queued    State = "queued"
	Leased    State = "leased"
	Done      State = "done"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

type Job struct {
	ID          int64
	NoteID      int64
	Kind        notes.Kind
	State       State
	Params      string
	Attempt     int
	MaxAttempts int
	LeaseToken  string
	LeaseUntil  time.Time
	WorkerID    string
	Progress    string
	Error       string
}

// ErrActive: the note already has a queued or leased job.
var ErrActive = errors.New("note already has an active job")

// ErrLeaseLost: the caller's lease is gone (expired, reaped, cancelled,
// completed, or never existed). The worker must abandon the job.
var ErrLeaseLost = errors.New("lease lost")

func Enqueue(ctx context.Context, tx *sql.Tx, noteID int64, kind notes.Kind, params string, now time.Time) (int64, error) {
	if params == "" {
		params = "{}"
	}
	ts := store.Now(now)
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs (note_id, kind, state, params, created_at, updated_at)
		VALUES (?, ?, 'queued', ?, ?, ?)`, noteID, kind, params, ts, ts)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrActive
		}
		return 0, err
	}
	return res.LastInsertId()
}

// Active returns the note's queued or leased job, or nil.
func Active(ctx context.Context, tx *sql.Tx, noteID int64) (*Job, error) {
	j, err := scan(tx.QueryRowContext(ctx, selectJob+` WHERE note_id = ? AND state IN ('queued','leased')`, noteID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ClaimNext leases the oldest claimable job of one of the given kinds.
// Returns nil, nil when there is none.
func ClaimNext(ctx context.Context, tx *sql.Tx, kinds []notes.Kind, workerID string, now time.Time, lease time.Duration) (*Job, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	q := `SELECT id FROM jobs WHERE state = 'queued' AND (not_before IS NULL OR not_before <= ?)
		AND kind IN (?` + strings.Repeat(",?", len(kinds)-1) + `) ORDER BY id LIMIT 1`
	args := []any{store.Now(now)}
	for _, k := range kinds {
		args = append(args, string(k))
	}
	var id int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	token := newToken()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'leased', attempt = attempt + 1,
		lease_token = ?, lease_expires_at = ?, worker_id = ?, progress = NULL, updated_at = ?
		WHERE id = ?`, token, store.Now(now.Add(lease)), workerID, store.Now(now), id); err != nil {
		return nil, err
	}
	return byID(ctx, tx, id)
}

// CheckLease returns the job if token holds a live lease on it, else ErrLeaseLost.
// An expired lease is lost even before the reaper notices: the rule is
// "past lease_expires_at, it isn't yours", with no grace window to reason about.
func CheckLease(ctx context.Context, tx *sql.Tx, id int64, token string, now time.Time) (*Job, error) {
	j, err := byID(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLeaseLost
	}
	if err != nil {
		return nil, err
	}
	if j.State != Leased || token == "" || j.LeaseToken != token || !now.Before(j.LeaseUntil) {
		return nil, ErrLeaseLost
	}
	return j, nil
}

func Extend(ctx context.Context, tx *sql.Tx, id int64, progress string, now time.Time, lease time.Duration) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = ?, progress = ?, updated_at = ? WHERE id = ?`,
		store.Now(now.Add(lease)), nullable(progress), store.Now(now), id)
	return err
}

// Finish moves a job to a terminal state and drops its lease.
func Finish(ctx context.Context, tx *sql.Tx, id int64, st State, errMsg string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, error = ?, lease_token = NULL,
		lease_expires_at = NULL, updated_at = ? WHERE id = ?`, st, nullable(errMsg), store.Now(now), id)
	return err
}

// Requeue puts a failed attempt back in the queue after a backoff:
// 30 s after the first attempt, 60 s after the second, and so on.
func Requeue(ctx context.Context, tx *sql.Tx, j *Job, errMsg string, now time.Time) error {
	backoff := 30 * time.Second << max(j.Attempt-1, 0)
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', error = ?, not_before = ?,
		lease_token = NULL, lease_expires_at = NULL, worker_id = NULL, updated_at = ? WHERE id = ?`,
		nullable(errMsg), store.Now(now.Add(backoff)), store.Now(now), j.ID)
	return err
}

// Expired lists leased jobs whose lease has run out.
func Expired(ctx context.Context, tx *sql.Tx, now time.Time) ([]*Job, error) {
	rows, err := tx.QueryContext(ctx, selectJob+` WHERE state = 'leased' AND lease_expires_at <= ?`, store.Now(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

const selectJob = `SELECT id, note_id, kind, state, params, attempt, max_attempts,
	COALESCE(lease_token, ''), COALESCE(lease_expires_at, ''), COALESCE(worker_id, ''),
	COALESCE(progress, ''), COALESCE(error, '') FROM jobs`

func byID(ctx context.Context, tx *sql.Tx, id int64) (*Job, error) {
	return scan(tx.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
}

type scanner interface{ Scan(...any) error }

func scan(r scanner) (*Job, error) {
	var j Job
	var until string
	if err := r.Scan(&j.ID, &j.NoteID, &j.Kind, &j.State, &j.Params, &j.Attempt, &j.MaxAttempts,
		&j.LeaseToken, &until, &j.WorkerID, &j.Progress, &j.Error); err != nil {
		return nil, err
	}
	if until != "" {
		t, err := store.ParseTime(until)
		if err != nil {
			return nil, err
		}
		j.LeaseUntil = t
	}
	return &j, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Notifier wakes long-polling claimers when a job is enqueued. Broadcast
// by closing a channel; waiters grab the current channel before checking
// the database, so a job enqueued between their check and their wait
// still wakes them.
type Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func NewNotifier() *Notifier { return &Notifier{ch: make(chan struct{})} }

func (n *Notifier) C() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *Notifier) Broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}
