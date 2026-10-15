package web

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/core"
)

// RSS 2.0 with the iTunes and Podcasting 2.0 namespaces: what Apple
// Podcasts, Overcast and Pocket Casts all read.
type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Itunes  string   `xml:"xmlns:itunes,attr"`
	Podcast string   `xml:"xmlns:podcast,attr"`
	Atom    string   `xml:"xmlns:atom,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Self        atomLink  `xml:"atom:link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Author      string    `xml:"itunes:author"`
	Image       hrefAttr  `xml:"itunes:image"`
	Explicit    string    `xml:"itunes:explicit"`
	Type        string    `xml:"itunes:type"`
	Block       string    `xml:"itunes:block"` // keep out of Apple's directory
	Locked      string    `xml:"podcast:locked"`
	Category    hrefText  `xml:"itunes:category"`
	Items       []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type hrefAttr struct {
	Href string `xml:"href,attr"`
}

type hrefText struct {
	Text string `xml:"text,attr"`
}

type rssItem struct {
	Title       string     `xml:"title"`
	Description cdata      `xml:"description"`
	GUID        guid       `xml:"guid"`
	PubDate     string     `xml:"pubDate"`
	Enclosure   enclosure  `xml:"enclosure"`
	Duration    int        `xml:"itunes:duration"`
	EpisodeType string     `xml:"itunes:episodeType"`
	Chapters    *chapterEl `xml:"podcast:chapters,omitempty"`
}

type cdata struct {
	Text string `xml:",cdata"`
}

type guid struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type chapterEl struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

func (h *Web) feedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /feed/{token}/all.xml", h.feed)
	mux.HandleFunc("GET /feed/{token}/c/{file}", h.feed)
	mux.HandleFunc("GET /feed/{token}/art.png", h.feedArt)
	mux.HandleFunc("GET /feed/{token}/a/{pid}/{file}", h.feedAudio)
	mux.HandleFunc("GET /feed/{token}/chapters/{pid}/{file}", h.feedChapters)
}

// tokenOK answers 404 for a bad token: don't confirm the path exists.
func (h *Web) tokenOK(w http.ResponseWriter, r *http.Request) bool {
	if !h.Svc.FeedTokenValid(r.Context(), r.PathValue("token")) {
		http.NotFound(w, r)
		return false
	}
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "private, max-age=0") // keeps it out of Cloudflare's cache
	return true
}

func (h *Web) feed(w http.ResponseWriter, r *http.Request) {
	if !h.tokenOK(w, r) {
		return
	}
	ctx := r.Context()
	tok := r.PathValue("token")
	base := h.Origin + "/feed/" + tok
	title, self := "notecast", base+"/all.xml"
	var coll *int64
	if file := r.PathValue("file"); file != "" {
		id, err := strconv.ParseInt(strings.TrimSuffix(file, ".xml"), 10, 64)
		if err != nil || !strings.HasSuffix(file, ".xml") {
			http.NotFound(w, r)
			return
		}
		name, err := h.Svc.CollectionName(ctx, id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		coll, title, self = &id, name+" · notecast", fmt.Sprintf("%s/c/%d.xml", base, id)
	}
	st, err := h.Svc.Settings(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.Svc.FeedItems(ctx, coll, st.FeedIncludeTranscript)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	doc := rss{
		Version: "2.0",
		Itunes:  "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Podcast: "https://podcastindex.org/namespace/1.0",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: channel{
			Title: title, Link: h.Origin, Description: "Narrated study notes.",
			Self:     atomLink{Href: self, Rel: "self", Type: "application/rss+xml"},
			Language: "en-us", Author: "notecast", Image: hrefAttr{base + "/art.png"},
			Explicit: "false", Type: "episodic", Block: "Yes", Locked: "yes",
			Category: hrefText{"Education"},
		},
	}
	for _, it := range items {
		ri := rssItem{
			Title:       orUntitled(it.Title),
			Description: cdata{describe(it)},
			GUID:        guid{IsPermaLink: "false", Value: fmt.Sprintf("%s-v%d", it.PID, it.AudioVersion)},
			PubDate:     it.Published.UTC().Format(time.RFC1123Z),
			Enclosure:   enclosure{fmt.Sprintf("%s/a/%s/v%d.mp3", base, it.PID, it.AudioVersion), it.AudioBytes, "audio/mpeg"},
			Duration:    int(it.AudioSeconds + 0.5),
			EpisodeType: "full",
		}
		if it.HasChapters {
			ri.Chapters = &chapterEl{fmt.Sprintf("%s/chapters/%s/v%d.json", base, it.PID, it.AudioVersion), "application/json+chapters"}
		}
		doc.Channel.Items = append(doc.Channel.Items, ri)
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	w.Write(out)
}

func orUntitled(s string) string {
	if s == "" {
		return "Untitled note"
	}
	return s
}

// describe is the episode's show notes: plain text, escaped by the encoder
// even inside CDATA (which only needs "]]>" handled; encoding/xml does).
func describe(it core.FeedItem) string {
	var b strings.Builder
	b.WriteString(it.Summary)
	if it.CollectionName != "" {
		fmt.Fprintf(&b, "\n\nCollection: %s", it.CollectionName)
	}
	if len(it.Tags) > 0 {
		fmt.Fprintf(&b, "\nTags: %s", strings.Join(it.Tags, ", "))
	}
	if it.Transcript != "" {
		b.WriteString("\n\n")
		b.WriteString(pageMarker.ReplaceAllString(it.Transcript, "— page $1 —"))
	}
	return b.String()
}

// versioned parses "v<N>.<ext>" and checks N is the note's current audio.
func (h *Web) versioned(w http.ResponseWriter, r *http.Request, ext string) (*core.Note, bool) {
	n, err := h.Svc.Get(r.Context(), r.PathValue("pid"))
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	file := r.PathValue("file")
	v, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(file, "v"), "."+ext), 10, 64)
	if err != nil || file != fmt.Sprintf("v%d.%s", v, ext) || v != n.AudioVersion || v == 0 {
		http.NotFound(w, r)
		return nil, false
	}
	return n, true
}

func (h *Web) feedAudio(w http.ResponseWriter, r *http.Request) {
	if !h.tokenOK(w, r) {
		return
	}
	n, ok := h.versioned(w, r, "mp3")
	if !ok {
		return
	}
	f, err := os.Open(h.Svc.Files().Audio(n.PID, n.AudioVersion))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (h *Web) feedChapters(w http.ResponseWriter, r *http.Request) {
	if !h.tokenOK(w, r) {
		return
	}
	n, ok := h.versioned(w, r, "json")
	if !ok {
		return
	}
	b, err := h.Svc.ChaptersJSON(r.Context(), n.PID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json+chapters")
	w.Write(b)
}

func (h *Web) feedArt(w http.ResponseWriter, r *http.Request) {
	if !h.tokenOK(w, r) {
		return
	}
	b, err := staticFS.ReadFile("static/art-1400.png")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(b)
}

func (h *Web) rotateFeed(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Svc.RotateFeedToken(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings#feed", http.StatusSeeOther)
}
