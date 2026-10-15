package alert

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/files"
	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/store"
)

func TestFiresOncePerOutage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, _ := store.Open(ctx, dir+"/app.db")
	defer db.Close()
	fs, _ := files.New(dir)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	svc := core.New(db, fs, func() time.Time { return now })

	var sent []string
	w := &Watcher{Svc: svc, Threshold: 2 * time.Hour, Now: func() time.Time { return now },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notify: func(_ context.Context, _, body string) error { sent = append(sent, body); return nil }}

	check := func(want int) {
		t.Helper()
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if len(sent) != want {
			t.Fatalf("sent %d alerts, want %d: %q", len(sent), want, sent)
		}
	}

	check(0) // nothing queued
	pid, _ := svc.CreateNote(ctx, nil)
	svc.AddPage(ctx, pid, bytesReader(jpeg))
	svc.Submit(ctx, pid)
	check(0) // queued, but not for long
	now = now.Add(3 * time.Hour)
	check(1) // worker never seen, job waited 3h
	check(1) // no repeat during the same outage

	// Worker comes back (a claim for kinds it can't serve still counts as seen)...
	svc.Claim(ctx, core.ClaimRequest{WorkerInfo: core.WorkerInfo{ID: "w"}, Kinds: []notes.Kind{notes.Speak}}, time.Millisecond)
	check(1)
	// ...and vanishes again for over the threshold: one new alert.
	now = now.Add(3 * time.Hour)
	check(2)
}

var jpeg = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
