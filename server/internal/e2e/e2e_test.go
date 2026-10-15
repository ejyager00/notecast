// Package e2e drives the whole note lifecycle over HTTP: the public form
// endpoints on one side, the fake worker on the worker API on the other.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/auth"
	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/fakeworker"
	"git.ericbits.win/eric/notecast/server/internal/files"
	"git.ericbits.win/eric/notecast/server/internal/jobs"
	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/store"
	"git.ericbits.win/eric/notecast/server/internal/web"
	"git.ericbits.win/eric/notecast/server/internal/workerapi"
)

const (
	origin = "https://notecast.test"
	token  = "worker-secret"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t      *testing.T
	db     *sql.DB
	svc    *core.Service
	clock  *clock
	public *httptest.Server
	worker *fakeworker.Client
	http   *http.Client
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir+"/app.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fs, err := files.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	svc := core.New(db, fs, clk.Now)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	app, open, err := (&web.Web{Svc: svc, Log: log, Version: "test"}).Handlers()
	if err != nil {
		t.Fatal(err)
	}
	pub := auth.Exempt(web.OpenPrefixes, open, auth.Dev("dev@example.com", auth.SameOrigin(origin, app)))
	public := httptest.NewServer(pub)
	t.Cleanup(public.Close)
	sum := sha256.Sum256([]byte(token))
	wapi := httptest.NewServer((&workerapi.API{Svc: svc, TokenSHA256: sum[:], Wait: 150 * time.Millisecond, Log: log}).Handler())
	t.Cleanup(wapi.Close)

	return &env{
		t: t, db: db, svc: svc, clock: clk, public: public,
		worker: &fakeworker.Client{BaseURL: wapi.URL, Token: token, ID: "test-worker"},
		http:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// post sends a same-origin form POST and returns status and Location.
func (e *env) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.public.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func (e *env) mustPost(path string, form url.Values) {
	e.t.Helper()
	if code, _ := e.post(path, form); code != http.StatusSeeOther {
		e.t.Fatalf("POST %s: %d", path, code)
	}
}

func (e *env) upload(pid string, data []byte) int {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("page", "IMG_0001.jpeg")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.public.URL+"/notes/"+pid+"/pages", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", origin)
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) note(pid string) *core.Note {
	e.t.Helper()
	n, err := e.svc.Get(context.Background(), pid)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) newNote(pages int) string {
	e.t.Helper()
	_, loc := e.post("/notes", nil)
	pid := strings.TrimPrefix(loc, "/notes/")
	for range pages {
		if code := e.upload(pid, fakeworker.TinyJPEG()); code != http.StatusSeeOther {
			e.t.Fatalf("upload: %d", code)
		}
	}
	return pid
}

func (e *env) process(kinds ...notes.Kind) {
	e.t.Helper()
	ok, err := e.worker.Process(kinds...)
	if err != nil || !ok {
		e.t.Fatalf("process %v: ok=%v err=%v", kinds, ok, err)
	}
}

func wantStatus(t *testing.T, n *core.Note, s notes.Status) {
	t.Helper()
	if n.Status != s {
		t.Fatalf("note status %s, want %s (error %q)", n.Status, s, n.Error)
	}
}

func lostLease(err error) bool {
	var se *fakeworker.StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict
}

func TestFullLifecycle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	_, loc := e.post("/notes", nil)
	pid := strings.TrimPrefix(loc, "/notes/")
	jpg := fakeworker.TinyJPEG()
	heic := append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), make([]byte, 100)...)
	if c := e.upload(pid, jpg); c != http.StatusSeeOther {
		t.Fatalf("jpeg upload %d", c)
	}
	if c := e.upload(pid, heic); c != http.StatusSeeOther {
		t.Fatalf("heic upload %d", c)
	}
	if c := e.upload(pid, []byte("<html>not an image</html>")); c != http.StatusBadRequest {
		t.Fatalf("non-image upload: %d, want 400", c)
	}
	if code, _ := e.post("/notes/"+pid+"/approve", nil); code != http.StatusConflict {
		t.Fatalf("approve while uploading: %d", code)
	}

	e.mustPost("/notes/"+pid+"/submit", nil)
	n := e.note(pid)
	wantStatus(t, n, notes.Transcribing)
	if n.Job == nil || n.Job.State != jobs.Queued {
		t.Fatalf("job %+v", n.Job)
	}

	// The partial unique index: a second active job for the note is impossible.
	tx, _ := e.db.BeginTx(ctx, nil)
	if _, err := jobs.Enqueue(ctx, tx, n.ID, notes.Narrate, "", e.clock.Now()); !errors.Is(err, jobs.ErrActive) {
		t.Fatalf("second active job: %v", err)
	}
	tx.Rollback()

	// Worker claims and fetches inputs byte-for-byte.
	job, err := e.worker.Claim(notes.Transcribe)
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	if len(job.Transcribe) != 2 || len(job.Pages) != 2 {
		t.Fatalf("claim payload %+v", job)
	}
	got, err := e.worker.Input(job, job.Pages[1].Input)
	if err != nil || !bytes.Equal(got, heic) {
		t.Fatalf("input mismatch: %v", err)
	}
	bad := *job
	bad.LeaseToken = "nope"
	if _, err := e.worker.Input(&bad, job.Pages[0].Input); !lostLease(err) {
		t.Fatalf("input with wrong lease: %v", err)
	}

	// A stale completion is rejected and changes nothing.
	res, out := fakeworker.Canned(job)
	if err := e.worker.Complete(&bad, res, out); !lostLease(err) {
		t.Fatalf("stale complete: %v", err)
	}
	n = e.note(pid)
	wantStatus(t, n, notes.Transcribing)
	if n.Pages[0].Transcript != nil || n.Title != "" {
		t.Fatal("stale completion changed the note")
	}

	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	n = e.note(pid)
	wantStatus(t, n, notes.Review)
	if n.Title != "Glycolysis basics" || n.Pages[1].Transcript == nil || !n.Pages[1].HasDisplay || n.Job != nil {
		t.Fatalf("after transcribe: %+v", n)
	}
	if _, err := os.Stat(e.svc.Files().PageDisplay(pid, n.Pages[0].ID)); err != nil {
		t.Fatal("display image not written")
	}
	if n.Provenance["transcribe"] == nil {
		t.Fatal("provenance not recorded")
	}

	// Search finds the note by a word only in the transcript, with a prefix.
	hits, err := e.svc.Search(ctx, "pyruv", nil)
	if err != nil || len(hits) != 1 || hits[0].PID != pid || !strings.Contains(hits[0].Snippet, "\x02") {
		t.Fatalf("search transcript: %+v %v", hits, err)
	}
	if hits, _ := e.svc.Search(ctx, "mitochondria", nil); len(hits) != 0 {
		t.Fatal("search matched text that isn't there yet")
	}

	// Editing the transcript makes the narration stale; approve refuses it.
	e.mustPost("/notes/"+pid+"/edit", url.Values{
		fmt.Sprintf("transcript-%d", n.Pages[0].ID): {"# Glycolysis\r\n- fixed reading"},
	})
	n = e.note(pid)
	if !n.NarrationStale || *n.Pages[0].Transcript != "# Glycolysis\n- fixed reading" {
		t.Fatalf("stale=%v transcript=%q", n.NarrationStale, *n.Pages[0].Transcript)
	}
	if code, _ := e.post("/notes/"+pid+"/approve", nil); code != http.StatusConflict {
		t.Fatalf("approve stale narration: %d, want 409", code)
	}

	// Rewrite -> narrate -> review, no longer stale.
	e.mustPost("/notes/"+pid+"/rewrite", nil)
	wantStatus(t, e.note(pid), notes.Narrating)
	e.process(notes.Narrate)
	n = e.note(pid)
	wantStatus(t, n, notes.Review)
	if n.NarrationStale || n.Title != "Glycolysis basics" {
		t.Fatalf("after narrate: stale=%v title=%q (user/first title must win)", n.NarrationStale, n.Title)
	}
	if hits, _ := e.svc.Search(ctx, "mitochondria", nil); len(hits) != 1 {
		t.Fatal("search didn't pick up the new narration")
	}

	// Approve -> speak -> ready.
	e.mustPost("/notes/"+pid+"/approve", nil)
	e.process(notes.Speak)
	n = e.note(pid)
	wantStatus(t, n, notes.Ready)
	if n.AudioVersion != 1 || n.AudioSeconds != 12.5 || len(n.Chapters) != 1 {
		t.Fatalf("audio: %+v", n)
	}
	v1 := e.svc.Files().Audio(pid, 1)
	if _, err := os.Stat(v1); err != nil {
		t.Fatal("audio not written")
	}

	// Editing the narration of a ready note goes back to review with the old
	// audio still live; re-speaking replaces it.
	e.mustPost("/notes/"+pid+"/edit", url.Values{"narration": {"## Glycolysis\nShorter."}})
	n = e.note(pid)
	wantStatus(t, n, notes.Review)
	if n.AudioVersion != 1 {
		t.Fatal("audio dropped on edit")
	}
	e.mustPost("/notes/"+pid+"/approve", nil)
	e.process(notes.Speak)
	n = e.note(pid)
	wantStatus(t, n, notes.Ready)
	if n.AudioVersion != 2 {
		t.Fatalf("audio version %d", n.AudioVersion)
	}
	if _, err := os.Stat(v1); !os.IsNotExist(err) {
		t.Fatal("superseded audio not removed")
	}

	// A title-only edit leaves a ready note ready.
	e.mustPost("/notes/"+pid+"/edit", url.Values{"title": {"My glycolysis"}})
	wantStatus(t, e.note(pid), notes.Ready)

	e.mustPost("/notes/"+pid+"/delete", nil)
	if _, err := e.svc.Get(ctx, pid); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("note not deleted")
	}
	if hits, _ := e.svc.Search(ctx, "glycolysis", nil); len(hits) != 0 {
		t.Fatal("deleted note still searchable")
	}
}

func TestLeaseExpiryRetriesThenFails(t *testing.T) {
	e := setup(t)
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)

	for attempt := 1; attempt <= 3; attempt++ {
		job, err := e.worker.Claim(notes.Transcribe)
		if err != nil || job == nil || job.Attempt != attempt {
			t.Fatalf("attempt %d: claim %+v %v", attempt, job, err)
		}
		e.clock.Advance(core.DefaultLease + time.Second)
		if err := e.worker.Heartbeat(job, "late"); !lostLease(err) {
			t.Fatalf("heartbeat after expiry: %v", err)
		}
		if n, err := e.svc.Reap(context.Background()); err != nil || n != 1 {
			t.Fatalf("reap: %d %v", n, err)
		}
		if attempt < 3 {
			wantStatus(t, e.note(pid), notes.Transcribing)
			// Backoff: not claimable until it passes.
			if j, _ := e.worker.Claim(notes.Transcribe); j != nil {
				t.Fatalf("attempt %d: claimable during backoff", attempt)
			}
			e.clock.Advance(30 * time.Second << (attempt - 1))
		}
	}
	n := e.note(pid)
	wantStatus(t, n, notes.Failed)
	if n.FailedStage != notes.Transcribe || !strings.Contains(n.Error, "lease expired") {
		t.Fatalf("failed note: stage=%q err=%q", n.FailedStage, n.Error)
	}

	e.mustPost("/notes/"+pid+"/retry", nil)
	wantStatus(t, e.note(pid), notes.Transcribing)
	e.process(notes.Transcribe)
	wantStatus(t, e.note(pid), notes.Review)
}

func TestCancelAbortsWorker(t *testing.T) {
	e := setup(t)
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)
	job, _ := e.worker.Claim(notes.Transcribe)
	if err := e.worker.Heartbeat(job, "page 1 of 1"); err != nil {
		t.Fatal(err)
	}
	e.mustPost("/notes/"+pid+"/cancel", nil)
	if err := e.worker.Heartbeat(job, "still going"); !lostLease(err) {
		t.Fatalf("heartbeat after cancel: %v", err)
	}
	res, out := fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); !lostLease(err) {
		t.Fatalf("complete after cancel: %v", err)
	}
	wantStatus(t, e.note(pid), notes.Uploading)
}

func TestNonRetryableFailure(t *testing.T) {
	e := setup(t)
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)
	job, _ := e.worker.Claim(notes.Transcribe)
	if err := e.worker.Fail(job, "image is unreadable", false); err != nil {
		t.Fatal(err)
	}
	n := e.note(pid)
	wantStatus(t, n, notes.Failed)
	if n.Error != "image is unreadable" {
		t.Fatalf("error %q", n.Error)
	}
}

func TestAutoApproveQueuesSpeech(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	st, _ := e.svc.Settings(ctx)
	st.AutoApprove = true
	if err := e.svc.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)
	e.process(notes.Transcribe)
	n := e.note(pid)
	wantStatus(t, n, notes.Speaking)
	if n.Job == nil || n.Job.Kind != notes.Speak {
		t.Fatalf("auto-approve didn't queue speak: %+v", n.Job)
	}
	// The worker only offers what it can do: a GPU-less claim still gets speak.
	e.process(notes.Speak)
	wantStatus(t, e.note(pid), notes.Ready)
}

func TestKindsFilter(t *testing.T) {
	e := setup(t)
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)
	if j, err := e.worker.Claim(notes.Speak); j != nil || err != nil {
		t.Fatalf("speak-only claim got %+v %v", j, err)
	}
}

func TestAddPagesLater(t *testing.T) {
	e := setup(t)
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)
	e.process(notes.Transcribe)
	if c := e.upload(pid, fakeworker.TinyJPEG()); c != http.StatusSeeOther {
		t.Fatalf("add page in review: %d", c)
	}
	e.mustPost("/notes/"+pid+"/transcribe-new", nil)
	job, _ := e.worker.Claim(notes.Transcribe)
	if len(job.Transcribe) != 1 || len(job.Pages) != 2 || job.Pages[0].Transcript == nil {
		t.Fatalf("only the new page should be transcribed: %+v", job)
	}
}

func TestAuthAndOrigin(t *testing.T) {
	e := setup(t)
	// Worker API without the right token.
	c := *e.worker
	c.Token = "wrong"
	var se *fakeworker.StatusError
	if _, err := c.Claim(notes.Transcribe); !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %v", err)
	}
	// Cross-origin POST.
	req, _ := http.NewRequest("POST", e.public.URL+"/notes", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", resp.StatusCode)
	}
}
