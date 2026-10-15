package e2e

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

var (
	inputRe    = regexp.MustCompile(`<input[^>]*\bname="([^"]+)"[^>]*\bvalue="([^"]*)"`)
	textareaRe = regexp.MustCompile(`(?s)<textarea[^>]*\bname="([^"]+)"[^>]*>(.*?)</textarea>`)
)

// form is the review form as a browser would submit it untouched.
func (e *env) form(pid string) url.Values {
	e.t.Helper()
	body := e.page("/notes/" + pid)
	f := url.Values{}
	for _, m := range inputRe.FindAllStringSubmatch(body, -1) {
		f.Set(m[1], html.UnescapeString(m[2]))
	}
	for _, m := range textareaRe.FindAllStringSubmatch(body, -1) {
		f.Set(m[1], strings.TrimPrefix(html.UnescapeString(m[2]), "\n"))
	}
	return f
}

// A form left open doesn't undo a change made since it was loaded: fields
// she didn't touch are skipped, and one changed on both sides is refused.
func TestStaleFormKeepsNewerEdits(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	pid := e.newNote(2)
	e.mustPost("/notes/"+pid+"/submit", nil)
	e.process(notes.Transcribe)
	n := e.note(pid)
	p1, p2 := n.Pages[0].ID, n.Pages[1].ID
	t1, n2 := fmt.Sprintf("transcript-%d", p1), fmt.Sprintf("narration-%d", p2)

	stale := e.form(pid)
	if stale.Get("seen-"+t1) != core.Fingerprint(*n.Pages[0].Transcript) || stale.Get("seen-title") == "" {
		t.Fatalf("form lacks fingerprints: %v", stale)
	}
	// Page 1 is fixed elsewhere; the open form then saves her page 2 edit.
	if err := e.svc.SaveEdits(ctx, pid, core.Edits{Transcripts: map[int64]string{p1: "fixed elsewhere"}}); err != nil {
		t.Fatal(err)
	}
	stale.Set(n2, "Her words.")
	e.mustPost("/notes/"+pid+"/edit", stale)
	n = e.note(pid)
	if *n.Pages[0].Transcript != "fixed elsewhere" || pageNarration(n, 1) != "Her words." || !n.Pages[1].NarrationEdited {
		t.Fatalf("after stale save: %q | %q", *n.Pages[0].Transcript, pageNarration(n, 1))
	}

	// Both sides change page 2's narration: nothing from the form is saved.
	stale = e.form(pid)
	if err := e.svc.SaveEdits(ctx, pid, core.Edits{Narrations: map[int64]string{p2: "Edited in another tab."}}); err != nil {
		t.Fatal(err)
	}
	stale.Set(n2, "Edited here.")
	stale.Set("title", "New title")
	code, loc := e.post("/notes/"+pid+"/edit", stale)
	if code != 303 || !strings.Contains(loc, "msg=") {
		t.Fatalf("conflict: %d %s", code, loc)
	}
	msg, _ := url.QueryUnescape(strings.SplitN(loc, "msg=", 2)[1])
	if !strings.Contains(msg, "Page 2's narration was changed somewhere else") {
		t.Fatalf("message: %q", msg)
	}
	if n = e.note(pid); pageNarration(n, 1) != "Edited in another tab." || n.Title == "New title" {
		t.Fatalf("conflict saved something: %q title %q", pageNarration(n, 1), n.Title)
	}

	// Saving the same text the other side saved is not a conflict.
	stale = e.form(pid)
	if err := e.svc.SaveEdits(ctx, pid, core.Edits{Narrations: map[int64]string{p2: "Same."}}); err != nil {
		t.Fatal(err)
	}
	stale.Set(n2, "Same.")
	stale.Set("title", "New title")
	e.mustPost("/notes/"+pid+"/edit", stale)
	if n = e.note(pid); n.Title != "New title" {
		t.Fatalf("title %q", n.Title)
	}
}
