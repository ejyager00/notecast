package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"git.ericbits.win/eric/notecast/server/internal/fakeworker"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

// fakeHEIC passes the upload sniffer as HEIC; nothing ever decodes it here.
func fakeHEIC() []byte {
	b := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'}
	return append(b, make([]byte, 64)...)
}

func pair(id int64) map[string][]byte {
	return map[string][]byte{
		fmt.Sprintf("display-%d.jpg", id): fakeworker.TinyJPEG(),
		fmt.Sprintf("thumb-%d.jpg", id):   fakeworker.TinyJPEG(),
	}
}

// Previews arrive while OCR is still running: the page shows them, a stale
// worker can't write them, and the job still completes normally after.
func TestPreviewsDuringTranscribe(t *testing.T) {
	e := setup(t)
	_, loc := e.post("/notes", nil)
	pid := strings.TrimPrefix(loc, "/notes/")
	for range 2 {
		if c := e.upload(pid, fakeHEIC()); c != http.StatusSeeOther {
			t.Fatalf("upload: %d", c)
		}
	}
	// Before any JPEG exists, non-Safari browsers get a placeholder.
	e.page("/notes/"+pid, `type="image/heic"`, "page-pending.svg")

	e.mustPost("/notes/"+pid+"/submit", nil)
	job, err := e.worker.Claim(notes.Transcribe)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	first, second := job.Transcribe[0], job.Transcribe[1]
	e.page("/notes/"+pid, "?previews=0")

	stale := *job
	stale.LeaseToken = "not-the-lease"
	if err := e.worker.Previews(&stale, pair(first)); !lostLease(err) {
		t.Fatalf("stale lease: got %v, want 409", err)
	}
	if err := e.worker.Previews(job, pair(first+second+100)); err == nil {
		t.Fatal("previews for a page outside the job were accepted")
	}
	if err := e.worker.Previews(job, map[string][]byte{fmt.Sprintf("display-%d.jpg", first): fakeworker.TinyJPEG()}); err == nil {
		t.Fatal("a display without its thumb was accepted")
	}
	if n := e.note(pid); n.Previews() != 0 {
		t.Fatalf("rejected uploads left %d previews", n.Previews())
	}

	if err := e.worker.Previews(job, pair(first)); err != nil {
		t.Fatal(err)
	}
	n := e.note(pid)
	wantStatus(t, n, notes.Transcribing)
	if !n.Pages[0].HasDisplay || n.Pages[1].HasDisplay {
		t.Fatalf("has_display = %v, %v; want true, false", n.Pages[0].HasDisplay, n.Pages[1].HasDisplay)
	}
	resp, _ := e.get(fmt.Sprintf("/notes/%s/pages/%d/thumb", pid, first))
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("thumb content type %q", ct)
	}
	// The open page polls with the count it rendered; a change reloads it.
	if resp, _ := e.get("/notes/"+pid+"/status?previews=0", "HX-Request", "true"); resp.Header.Get("HX-Refresh") != "true" {
		t.Fatal("new previews didn't trigger a refresh")
	}
	if resp, _ := e.get("/notes/"+pid+"/status?previews=1", "HX-Request", "true"); resp.Header.Get("HX-Refresh") != "" {
		t.Fatal("refresh with nothing new")
	}

	res, out := fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.note(pid), notes.Review)

	// Previews are for transcribe jobs only.
	e.mustPost("/notes/"+pid+"/approve", nil)
	speak, _ := e.worker.Claim(notes.Speak)
	if err := e.worker.Previews(speak, pair(first)); err == nil {
		t.Fatal("previews accepted on a speak job")
	}
}
