package e2e

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/fakeworker"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

// marked lists the page positions a claim asks the worker to narrate.
func marked(job *core.Claimed) []int {
	var out []int
	for _, p := range job.Pages {
		if p.Narrate {
			out = append(out, p.Position)
		}
	}
	return out
}

func (e *env) claimNarrate() *core.Claimed {
	e.t.Helper()
	job, err := e.worker.Claim(notes.Narrate)
	if err != nil || job == nil {
		e.t.Fatalf("claim narrate: %v %v", job, err)
	}
	return job
}

func pageNarration(n *core.Note, i int) string {
	if n.Pages[i].Narration == nil {
		return "<nil>"
	}
	return *n.Pages[i].Narration
}

// Narration is per page: a rewrite redoes only what changed, keeps her
// edits, and the note's script is the pages' parts in order.
func TestPageNarration(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	pid := e.newNote(3)
	e.mustPost("/notes/"+pid+"/submit", nil)
	job, _ := e.worker.Claim(notes.Transcribe)
	if got := marked(job); !slices.Equal(got, []int{1, 2, 3}) || !job.Meta {
		t.Fatalf("first pass narrates every page with meta: %v meta=%v", got, job.Meta)
	}
	res, out := fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	n := e.note(pid)
	if !n.Segmented() || n.NarrationStale || !strings.HasPrefix(n.Narration, "## Page 1\n") ||
		!strings.Contains(n.Narration, "\n\n## Page 3\n") {
		t.Fatalf("joined narration: stale=%v %q", n.NarrationStale, n.Narration)
	}

	// Fixing page 2's transcript marks only page 2.
	e.mustPost("/notes/"+pid+"/edit", url.Values{fmt.Sprintf("transcript-%d", n.Pages[1].ID): {"fixed"}})
	n = e.note(pid)
	if !n.NarrationStale || n.Pages[0].NarrationStale || !n.Pages[1].NarrationStale || n.Pages[2].NarrationStale {
		t.Fatalf("stale flags: note=%v pages=%v %v %v", n.NarrationStale,
			n.Pages[0].NarrationStale, n.Pages[1].NarrationStale, n.Pages[2].NarrationStale)
	}
	e.page("/notes/"+pid, "Rewrite changed pages", "You changed this page after its narration was written.")

	// She also hand-edits page 3's narration.
	e.mustPost("/notes/"+pid+"/edit", url.Values{fmt.Sprintf("narration-%d", n.Pages[2].ID): {"Her own words."}})

	e.mustPost("/notes/"+pid+"/rewrite", nil)
	job = e.claimNarrate()
	if got := marked(job); !slices.Equal(got, []int{2}) || job.Meta {
		t.Fatalf("rewrite after one fix: narrate %v meta=%v", got, job.Meta)
	}
	if job.Pages[0].Narration == nil || *job.Pages[0].Transcript == "" {
		t.Fatal("kept pages travel with their narration as context")
	}
	// A result missing the asked-for page, or adding one, is refused.
	if err := e.worker.Complete(job, core.Result{}, nil); err == nil {
		t.Fatal("completion without the page's narration accepted")
	}
	extra := core.Result{Segments: []core.SegmentResult{{PageID: job.Pages[1].PageID, Narration: "x"},
		{PageID: job.Pages[0].PageID, Narration: "y"}}}
	if err := e.worker.Complete(job, extra, nil); err == nil {
		t.Fatal("completion narrating a page not asked for accepted")
	}
	res, out = fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	n = e.note(pid)
	if n.NarrationStale || !strings.Contains(pageNarration(n, 1), "mitochondria") ||
		strings.Contains(pageNarration(n, 0), "mitochondria") || pageNarration(n, 2) != "Her own words." {
		t.Fatalf("after rewrite: %q | %q | %q", pageNarration(n, 0), pageNarration(n, 1), pageNarration(n, 2))
	}
	if n.Title != "Glycolysis basics" {
		t.Fatalf("one-page fix regenerated the title: %q", n.Title)
	}
	if !strings.HasSuffix(n.Narration, "\n\nHer own words.") {
		t.Fatalf("joined: %q", n.Narration)
	}

	// Nothing stale: "Rewrite narration" redoes every page except her edit.
	e.mustPost("/notes/"+pid+"/rewrite", nil)
	job = e.claimNarrate()
	if got := marked(job); !slices.Equal(got, []int{1, 2}) || !job.Meta {
		t.Fatalf("full rewrite: narrate %v meta=%v", got, job.Meta)
	}
	res, out = fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	if n = e.note(pid); pageNarration(n, 2) != "Her own words." || n.Summary != "Revised." {
		t.Fatalf("full rewrite: page 3 %q summary %q", pageNarration(n, 2), n.Summary)
	}

	// Emptying her box hands the page back to the model.
	e.mustPost("/notes/"+pid+"/edit", url.Values{fmt.Sprintf("narration-%d", n.Pages[2].ID): {""}})
	if n = e.note(pid); n.Pages[2].Narration != nil || !n.NarrationStale || strings.Contains(n.Narration, "Her own") {
		t.Fatalf("cleared: %q stale=%v", pageNarration(n, 2), n.NarrationStale)
	}
	e.mustPost("/notes/"+pid+"/rewrite", nil)
	job = e.claimNarrate()
	if got := marked(job); !slices.Equal(got, []int{3}) {
		t.Fatalf("after clearing page 3: narrate %v", got)
	}
	res, out = fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}

	// Moving or deleting a page reorders the script; nothing goes stale.
	n = e.note(pid)
	first := pageNarration(n, 0)
	if err := e.svc.MovePage(ctx, pid, n.Pages[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	n = e.note(pid)
	if n.NarrationStale || pageNarration(n, 1) != first || !strings.HasPrefix(n.Narration, pageNarration(n, 0)) {
		t.Fatalf("after move: stale=%v %q", n.NarrationStale, n.Narration)
	}
	if err := e.svc.DeletePage(ctx, pid, n.Pages[1].ID); err != nil {
		t.Fatal(err)
	}
	if n = e.note(pid); n.NarrationStale || strings.Contains(n.Narration, first) || len(n.Pages) != 2 {
		t.Fatalf("after delete: stale=%v %q", n.NarrationStale, n.Narration)
	}

	// An added page is read and narrated on its own, with a new title/summary pass.
	e.upload(pid, fakeworker.TinyJPEG())
	e.mustPost("/notes/"+pid+"/transcribe-new", nil)
	job, _ = e.worker.Claim(notes.Transcribe)
	if got := marked(job); len(job.Transcribe) != 1 || !slices.Equal(got, []int{job.Pages[2].Position}) || !job.Meta {
		t.Fatalf("added page: transcribe %v narrate %v meta=%v", job.Transcribe, got, job.Meta)
	}
	res, out = fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	if n = e.note(pid); n.NarrationStale || n.Pages[2].Narration == nil || n.Title != "Glycolysis basics" {
		t.Fatalf("after added page: stale=%v title=%q", n.NarrationStale, n.Title)
	}
}

// A note narrated before narration was per page keeps its one narration,
// and its first rewrite narrates every page.
func TestLegacyNarrationConverts(t *testing.T) {
	e := setup(t)
	pid := e.newNote(2)
	e.mustPost("/notes/"+pid+"/submit", nil)
	e.process(notes.Transcribe)
	n := e.note(pid)
	if _, err := e.db.Exec(`UPDATE pages SET narration = NULL WHERE note_id = ?`, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE notes SET narration = 'The old whole-note script.' WHERE id = ?`, n.ID); err != nil {
		t.Fatal(err)
	}
	if n = e.note(pid); !n.Legacy() {
		t.Fatal("not legacy")
	}
	e.page("/notes/"+pid, "The old whole-note script.", "written for the whole note at once")

	// Editing a transcript keeps the old script (marked stale) instead of
	// rebuilding it from empty page narrations.
	e.mustPost("/notes/"+pid+"/edit", url.Values{fmt.Sprintf("transcript-%d", n.Pages[0].ID): {"fixed"}})
	if n = e.note(pid); n.Narration != "The old whole-note script." || !n.NarrationStale {
		t.Fatalf("legacy edit: %q stale=%v", n.Narration, n.NarrationStale)
	}

	e.mustPost("/notes/"+pid+"/rewrite", nil)
	job := e.claimNarrate()
	if got := marked(job); !slices.Equal(got, []int{1, 2}) || !job.Meta {
		t.Fatalf("legacy rewrite: narrate %v meta=%v", got, job.Meta)
	}
	res, out := fakeworker.Canned(job)
	if err := e.worker.Complete(job, res, out); err != nil {
		t.Fatal(err)
	}
	if n = e.note(pid); n.Legacy() || n.NarrationStale || strings.Contains(n.Narration, "old whole-note") {
		t.Fatalf("after legacy rewrite: legacy=%v %q", n.Legacy(), n.Narration)
	}
}
