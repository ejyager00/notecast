// Package alert tells Eric when notes are stuck because the worker on
// roundabits is offline (ARCHITECTURE §4.8). It fires once per outage.
package alert

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/core"
)

type Watcher struct {
	Svc       *core.Service
	Notify    func(ctx context.Context, title, body string) error
	Threshold time.Duration // how long a job waits / the worker is silent before alerting
	Now       func() time.Time
	Log       *slog.Logger

	fired bool
}

// Check alerts at most once per outage: re-armed when the worker is seen again.
func (w *Watcher) Check(ctx context.Context) error {
	now := w.Now()
	oldest, ok, err := w.Svc.OldestQueued(ctx)
	if err != nil {
		return err
	}
	ws, err := w.Svc.Workers(ctx)
	if err != nil {
		return err
	}
	var lastSeen time.Time
	if len(ws) > 0 {
		lastSeen = ws[0].LastSeen
	}
	workerGone := now.Sub(lastSeen) > w.Threshold
	if !workerGone {
		w.fired = false
		return nil
	}
	if w.fired || !ok || now.Sub(oldest) < w.Threshold {
		return nil
	}
	pending, err := w.Svc.PendingJobs(ctx)
	if err != nil {
		return err
	}
	since := "never connected"
	if !lastSeen.IsZero() {
		since = "last seen " + lastSeen.Local().Format("Jan 2 15:04")
	}
	body := fmt.Sprintf("%d note(s) waiting; roundabits worker %s. Turn it on (or start notecast-worker).", pending, since)
	if err := w.Notify(ctx, "notecast", body); err != nil {
		return err
	}
	w.fired = true
	return nil
}

func (w *Watcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Check(ctx); err != nil {
				w.Log.Warn("alert check", "err", err)
			}
		}
	}
}

// Command notifies by running cmd with title and body as arguments, e.g. the
// box's rate-limited notify.sh.
func Command(cmd string) func(context.Context, string, string) error {
	return func(ctx context.Context, title, body string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, cmd, title, body).Run()
	}
}
