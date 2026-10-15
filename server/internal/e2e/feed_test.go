package e2e

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/fakeworker"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

type feedDoc struct {
	Channel struct {
		Title string `xml:"title"`
		Block string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd block"`
		Image struct {
			Href string `xml:"href,attr"`
		} `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
		Items []struct {
			Title       string `xml:"title"`
			Description string `xml:"description"`
			GUID        string `xml:"guid"`
			Enclosure   struct {
				URL    string `xml:"url,attr"`
				Length int64  `xml:"length,attr"`
				Type   string `xml:"type,attr"`
			} `xml:"enclosure"`
			Duration string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
		} `xml:"item"`
	} `xml:"channel"`
}

func (e *env) feed(path string) (int, feedDoc) {
	e.t.Helper()
	resp, body := e.get(path)
	var d feedDoc
	if resp.StatusCode == http.StatusOK {
		if err := xml.Unmarshal([]byte(body), &d); err != nil {
			e.t.Fatalf("feed XML: %v\n%s", err, body)
		}
	}
	return resp.StatusCode, d
}

func (e *env) readyNote(collection string, narration string) string {
	e.t.Helper()
	ctx := context.Background()
	var form url.Values
	if collection != "" {
		cols, _ := e.svc.Collections(ctx)
		for _, c := range cols {
			if c.Name == collection {
				form = url.Values{"collection_id": {fmt.Sprint(c.ID)}}
			}
		}
	}
	_, loc := e.post("/notes", form)
	pid := strings.TrimPrefix(loc, "/notes/")
	e.upload(pid, fakeworker.TinyJPEG())
	e.mustPost("/notes/"+pid+"/submit", nil)
	e.process(notes.Transcribe)
	if narration != "" {
		e.mustPost("/notes/"+pid+"/edit", url.Values{"narration": {narration}})
	}
	e.mustPost("/notes/"+pid+"/approve", nil)
	e.process(notes.Speak)
	return pid
}

func TestFeeds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	if code, _ := e.feed("/feed/anything/all.xml"); code != http.StatusNotFound {
		t.Fatalf("feed before a token exists: %d", code)
	}
	e.mustPost("/collections", url.Values{"name": {"Parasitology"}})
	e.mustPost("/settings/feed-token", nil)
	st, _ := e.svc.Settings(ctx)
	tok := st.FeedToken
	if len(tok) < 40 {
		t.Fatalf("token too short: %q", tok)
	}
	e.page("/settings", "/feed/"+tok+"/all.xml", "Parasitology", "Follow a Show by URL")

	p1 := e.readyNote("Parasitology", "")
	e.clock.Advance(time.Minute)
	p2 := e.readyNote("", "")
	pending := e.newNote(1) // no audio: never in a feed

	code, all := e.feed("/feed/" + tok + "/all.xml")
	if code != 200 || len(all.Channel.Items) != 2 || all.Channel.Block != "Yes" {
		t.Fatalf("all feed: %d items=%d block=%q", code, len(all.Channel.Items), all.Channel.Block)
	}
	it := all.Channel.Items[1] // newest first; p1 is older
	if it.GUID != p1+"-v1" || it.Enclosure.Type != "audio/mpeg" || it.Enclosure.Length == 0 || it.Duration != "13" ||
		!strings.HasPrefix(it.Enclosure.URL, origin+"/feed/"+tok+"/a/"+p1+"/v1.mp3") ||
		!strings.Contains(it.Description, "Collection: Parasitology") {
		t.Fatalf("item: %+v", it)
	}
	if strings.Contains(fmt.Sprint(all), pending) {
		t.Fatal("note without audio in feed")
	}

	cols, _ := e.svc.Collections(ctx)
	code, coll := e.feed(fmt.Sprintf("/feed/%s/c/%d.xml", tok, cols[0].ID))
	if code != 200 || len(coll.Channel.Items) != 1 || coll.Channel.Items[0].GUID != p1+"-v1" ||
		!strings.HasPrefix(coll.Channel.Title, "Parasitology") {
		t.Fatalf("collection feed: %d %+v", code, coll.Channel)
	}
	if code, _ := e.feed("/feed/" + tok + "/c/999.xml"); code != 404 {
		t.Fatalf("unknown collection: %d", code)
	}

	// Enclosure: no app auth needed, Range works, only the current version.
	enc := strings.TrimPrefix(it.Enclosure.URL, origin)
	req, _ := http.NewRequest("GET", e.public.URL+enc, nil)
	req.Header.Set("Range", "bytes=0-2")
	r, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusPartialContent || r.Header.Get("X-Robots-Tag") == "" {
		t.Fatalf("enclosure range: %d", r.StatusCode)
	}
	if r, _ := e.get(strings.Replace(enc, "v1.mp3", "v2.mp3", 1)); r.StatusCode != 404 {
		t.Fatalf("stale version: %d", r.StatusCode)
	}
	if r, _ := e.get("/feed/" + tok + "/art.png"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("art: %d", r.StatusCode)
	}

	// Re-speaking makes a new episode (new GUID) and drops the old one.
	e.mustPost("/notes/"+p2+"/edit", url.Values{
		fmt.Sprintf("narration-%d", e.note(p2).Pages[0].ID): {"## New\nDifferent words."}})
	e.mustPost("/notes/"+p2+"/approve", nil)
	e.process(notes.Speak)
	_, all = e.feed("/feed/" + tok + "/all.xml")
	if all.Channel.Items[0].GUID != p2+"-v2" || len(all.Channel.Items) != 2 {
		t.Fatalf("after re-speak: %+v", all.Channel.Items)
	}

	// Rotation kills every old URL.
	e.mustPost("/settings/feed-token", nil)
	if code, _ := e.feed("/feed/" + tok + "/all.xml"); code != 404 {
		t.Fatalf("old token still works: %d", code)
	}
	if r, _ := e.get(enc); r.StatusCode != 404 {
		t.Fatalf("old enclosure still works: %d", r.StatusCode)
	}
}
