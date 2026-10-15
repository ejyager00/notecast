package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"git.ericbits.win/eric/notecast/server/internal/fakeworker"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

func (e *env) get(path string, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.public.URL+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) page(path string, want ...string) string {
	e.t.Helper()
	resp, body := e.get(path)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s: %d\n%s", path, resp.StatusCode, body)
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			e.t.Errorf("GET %s: missing %q", path, w)
		}
	}
	return body
}

func TestPagesThroughLifecycle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	resp, _ := e.get("/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP missing: %q", csp)
	}
	e.page("/", "No notes yet", `href="/new"`)
	e.page("/new", "Next: add pages")
	e.page("/settings", "Never connected", "Heart (US, female)")
	e.page("/static/htmx.min.js", "htmx")
	if r, _ := e.get("/static/"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("static directory listing: %d", r.StatusCode)
	}

	e.mustPost("/collections", url.Values{"name": {"Hematology"}})
	cols, _ := e.svc.Collections(ctx)
	_, loc := e.post("/notes", url.Values{"collection_id": {fmt.Sprint(cols[0].ID)}})
	pid := strings.TrimPrefix(loc, "/notes/")
	e.page("/notes/"+pid, "Take or choose photos", "data-upload=")
	e.upload(pid, fakeworker.TinyJPEG())
	e.page("/notes/"+pid, "Read 1 page", "Hematology")

	e.mustPost("/notes/"+pid+"/submit", nil)
	e.page("/notes/"+pid, "Reading pages", "waiting for the processing machine to connect", `hx-get="/notes/`+pid+`/status"`)
	if r, body := e.get("/notes/"+pid+"/status", "HX-Request", "true"); r.Header.Get("HX-Refresh") != "" || strings.Contains(body, "<html") {
		t.Fatalf("status partial while running: refresh=%q", r.Header.Get("HX-Refresh"))
	}

	// Transcribe, with a hostile transcript to check escaping everywhere.
	job, _ := e.worker.Claim(notes.Transcribe)
	res, out := fakeworker.Canned(job)
	res.Pages[0].Transcript = "Taenia [?solium] <script>alert(1)</script> [illegible]"
	res.Narration.Title = `<img src=x onerror=alert(1)>`
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.get("/notes/"+pid+"/status", "HX-Request", "true"); r.Header.Get("HX-Refresh") != "true" {
		t.Fatal("status partial didn't ask for a refresh after the job finished")
	}
	body := e.page("/notes/"+pid, "Ready to review", "Make audio", "Check: <span>solium</span><span>illegible</span>")
	for _, bad := range []string{"<script>alert", "<img src=x"} {
		if strings.Contains(body, bad) {
			t.Fatalf("unescaped %q in note page", bad)
		}
	}

	// Search: htmx gets just the fragment, highlights are <mark>, text escaped.
	_, frag := e.get("/search?q=taen", "HX-Request", "true", "HX-Target", "results")
	if strings.Contains(frag, "<html") || !strings.Contains(frag, "<mark>Taenia</mark>") || strings.Contains(frag, "<script>alert") {
		t.Fatalf("search fragment:\n%s", frag)
	}
	e.page("/search?q=taen", "<html") // a full page without htmx

	// Edit transcript and press "Make audio" in one submit: edits are saved,
	// then approve is refused because the narration is now stale.
	n := e.note(pid)
	code, loc := e.post("/notes/"+pid+"/edit", url.Values{
		fmt.Sprintf("transcript-%d", n.Pages[0].ID): {"Taenia solium"},
		"then": {"approve"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "msg=") {
		t.Fatalf("stale approve: %d %s", code, loc)
	}
	if n := e.note(pid); *n.Pages[0].Transcript != "Taenia solium" || n.Status != notes.Review {
		t.Fatal("edits not saved before the refused action")
	}
	e.page(loc, "Saved.", "Rewrite narration", "Make audio anyway")

	e.mustPost("/notes/"+pid+"/edit", url.Values{"then": {"approve-anyway"}})
	e.process(notes.Speak)
	e.page("/notes/"+pid, `<audio controls`, "Download MP3", `data-rate="1.5"`)

	// Audio: ranges (iOS needs them) and download disposition.
	req, _ := http.NewRequest("GET", e.public.URL+"/notes/"+pid+"/audio", nil)
	req.Header.Set("Range", "bytes=0-9")
	r, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusPartialContent || r.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("range request: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	r, _ = e.get("/notes/" + pid + "/audio?download=1")
	if cd := r.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("disposition %q", cd)
	}
	r, _ = e.get(fmt.Sprintf("/notes/%s/pages/%d/display", pid, n.Pages[0].ID))
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("display image: %d", r.StatusCode)
	}

	e.page("/", "Hematology", "0:13") // listed with collection and duration
	e.page("/settings", "1 note")     // collection count

	if r, body := e.get("/notes/nonexistent1"); r.StatusCode != http.StatusNotFound || !strings.Contains(body, "Not found") {
		t.Fatalf("missing note: %d", r.StatusCode)
	}
}

func TestFailedNotePage(t *testing.T) {
	e := setup(t)
	pid := e.newNote(1)
	e.mustPost("/notes/"+pid+"/submit", nil)
	job, _ := e.worker.Claim(notes.Transcribe)
	e.worker.Fail(job, "model crashed", false)
	e.page("/notes/"+pid, "Something went wrong while reading the pages", "model crashed", "Try again")
}
