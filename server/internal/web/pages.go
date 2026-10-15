package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/notes"
	"git.ericbits.win/eric/notecast/server/internal/search"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// view is the data every page template gets; each page uses what it needs.
type view struct {
	Title         string
	Version       string
	Flash         string
	Back          string
	Query         string
	CollectionID  *int64
	Items         []core.ListItem
	Collections   []core.Collection
	Note          *core.Note
	JobLabel      string
	Untranscribed int
	Editable      bool
	Settings      core.Settings
	Worker        workerStatus
	Pending       int
	Feeds         []feedLink
}

type feedLink struct {
	ID   string
	Name string
	URL  string
}

type workerStatus struct {
	Online   bool
	State    string
	Detail   string
	LastSeen time.Time
}

// A worker long-polls every ~25 s, so silence for longer than this means it's gone.
const workerOnlineWindow = 90 * time.Second

type voice struct{ ID, Name string }

// Kokoro v1.0 English voices.
var voices = []voice{
	{"af_heart", "Heart (US, female)"}, {"af_bella", "Bella (US, female)"},
	{"af_nicole", "Nicole (US, female)"}, {"af_sarah", "Sarah (US, female)"},
	{"af_aoede", "Aoede (US, female)"}, {"af_kore", "Kore (US, female)"},
	{"af_nova", "Nova (US, female)"}, {"af_sky", "Sky (US, female)"},
	{"af_river", "River (US, female)"}, {"af_jessica", "Jessica (US, female)"},
	{"af_alloy", "Alloy (US, female)"},
	{"am_michael", "Michael (US, male)"}, {"am_fenrir", "Fenrir (US, male)"},
	{"am_puck", "Puck (US, male)"}, {"am_echo", "Echo (US, male)"},
	{"am_eric", "Eric (US, male)"}, {"am_liam", "Liam (US, male)"},
	{"am_onyx", "Onyx (US, male)"}, {"am_adam", "Adam (US, male)"},
	{"bf_emma", "Emma (UK, female)"}, {"bf_isabella", "Isabella (UK, female)"},
	{"bf_alice", "Alice (UK, female)"}, {"bf_lily", "Lily (UK, female)"},
	{"bm_george", "George (UK, male)"}, {"bm_fable", "Fable (UK, male)"},
	{"bm_lewis", "Lewis (UK, male)"}, {"bm_daniel", "Daniel (UK, male)"},
}

var flagRe = regexp.MustCompile(`\[\?([^\]\n]{1,60})\]|\[illegible\]`)

var funcs = template.FuncMap{
	"add": func(a, b int) int { return a + b },
	// seen is the hidden field that lets a save skip what she didn't change.
	"seen": func(field string, value any) template.HTML {
		var s string
		switch v := value.(type) {
		case string:
			s = v
		case *string:
			if v != nil {
				s = *v
			}
		}
		return template.HTML(`<input type="hidden" name="seen-` + template.HTMLEscapeString(field) +
			`" value="` + core.Fingerprint(s) + `">`)
	},
	"deref": func(v any) any {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer {
			return v
		}
		if rv.IsNil() {
			return reflect.Zero(rv.Type().Elem()).Interface()
		}
		return rv.Elem().Interface()
	},
	"statusLabel": statusLabel,
	"stageLabel": func(k notes.Kind) string {
		switch k {
		case notes.Transcribe:
			return "reading the pages"
		case notes.Narrate:
			return "writing the narration"
		case notes.Speak:
			return "making the audio"
		}
		return "processing"
	},
	"workerStateLabel": func(s string) string {
		switch s {
		case "busy":
			return "working"
		case "gpu_unavailable":
			return "waiting for the GPU"
		case "", "idle":
			return "idle"
		}
		return s
	},
	"date": func(t time.Time) string {
		t = t.Local()
		if t.Year() == time.Now().Year() {
			return t.Format("Jan 2")
		}
		return t.Format("Jan 2, 2006")
	},
	"ago": ago,
	"dur": dur,
	"snippet": func(s string) template.HTML {
		esc := template.HTMLEscapeString(s)
		esc = strings.ReplaceAll(esc, search.MarkStart, "<mark>")
		esc = strings.ReplaceAll(esc, search.MarkEnd, "</mark>")
		return template.HTML(esc) // built only from escaped text plus our own tags
	},
	"flags": func(s string) []string {
		var out []string
		seen := map[string]bool{}
		for _, m := range flagRe.FindAllStringSubmatch(s, 30) {
			w := m[1]
			if w == "" {
				w = "illegible"
			}
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
		return out
	},
	"speeds":     func() []string { return []string{"1", "1.25", "1.5", "1.75", "2"} },
	"readSpeeds": func() []float64 { return []float64{0.8, 0.9, 1, 1.1, 1.2, 1.3} },
	"voices":     func() []voice { return voices },
	"voiceName": func(id string) string {
		for _, v := range voices {
			if v.ID == id {
				return v.Name
			}
		}
		return id
	},
	"transcriptText": func(n *core.Note) string {
		return pageMarker.ReplaceAllString(n.Transcript(), "— page $1 —")
	},
}

var pageMarker = regexp.MustCompile(`<!-- page (\d+) -->`)

func statusLabel(s notes.Status) string {
	switch s {
	case notes.Uploading:
		return "Adding pages"
	case notes.Transcribing:
		return "Reading pages"
	case notes.Review:
		return "Ready to review"
	case notes.Narrating:
		return "Writing narration"
	case notes.Speaking:
		return "Making audio"
	case notes.Ready:
		return "Ready"
	case notes.Failed:
		return "Failed"
	}
	return string(s)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	}
	return "on " + t.Local().Format("Jan 2 at 15:04")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func dur(sec float64) string {
	s := int(sec + 0.5)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

type templates map[string]*template.Template

func loadTemplates() (templates, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, err
	}
	out := templates{}
	for _, page := range []string{"library", "new", "note", "settings", "error"} {
		t, err := template.Must(base.Clone()).ParseFS(templateFS, "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		out[page] = t
	}
	return out, nil
}

// render executes into a buffer first so a template error can't leave a
// half-written page.
func (h *Web) render(w http.ResponseWriter, r *http.Request, code int, page, tmpl string, v *view) {
	v.Version = h.Version
	var buf bytes.Buffer
	if err := h.tmpl[page].ExecuteTemplate(&buf, tmpl, v); err != nil {
		h.Log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	buf.WriteTo(w)
}

// userMsg strips the sentinel prefix ("conflict: ...") from a core error.
func userMsg(err error) string {
	msg := err.Error()
	for _, sentinel := range []error{core.ErrConflict, core.ErrInvalid, core.ErrNotFound} {
		if errors.Is(err, sentinel) {
			msg = strings.TrimPrefix(msg, sentinel.Error()+": ")
		}
	}
	if msg == "" {
		return "Something went wrong."
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

// backWithMessage returns to the note page and shows msg there.
func backWithMessage(w http.ResponseWriter, r *http.Request, pid, msg string) {
	http.Redirect(w, r, "/notes/"+pid+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (h *Web) common(r *http.Request) (*view, error) {
	ctx := r.Context()
	v := &view{}
	var err error
	if v.Collections, err = h.Svc.Collections(ctx); err != nil {
		return nil, err
	}
	if v.Settings, err = h.Svc.Settings(ctx); err != nil {
		return nil, err
	}
	ws, err := h.Svc.Workers(ctx)
	if err != nil {
		return nil, err
	}
	if len(ws) > 0 { // most recently seen first
		w := ws[0]
		v.Worker = workerStatus{Online: time.Since(w.LastSeen) < workerOnlineWindow, State: w.State, Detail: w.Detail, LastSeen: w.LastSeen}
	}
	if v.Pending, err = h.Svc.PendingJobs(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// --- pages -----------------------------------------------------------------------

func (h *Web) library(w http.ResponseWriter, r *http.Request) {
	v, err := h.common(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	if v.CollectionID, err = optionalID(r.URL.Query().Get("c")); err != nil {
		h.fail(w, r, err)
		return
	}
	if v.Items, err = h.Svc.Search(r.Context(), v.Query, v.CollectionID); err != nil {
		h.fail(w, r, err)
		return
	}
	tmpl := "layout"
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "results" {
		tmpl = "results"
	}
	h.render(w, r, http.StatusOK, "library", tmpl, v)
}

func (h *Web) newNote(w http.ResponseWriter, r *http.Request) {
	v, err := h.common(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Title = "New note"
	h.render(w, r, http.StatusOK, "new", "layout", v)
}

func (h *Web) noteView(r *http.Request) (*view, error) {
	v, err := h.common(r)
	if err != nil {
		return nil, err
	}
	n, err := h.Svc.Get(r.Context(), r.PathValue("pid"))
	if err != nil {
		return nil, err
	}
	v.Note = n
	v.Title = n.Title
	if v.Title == "" {
		v.Title = "Untitled note"
	}
	v.Flash = r.URL.Query().Get("msg")
	v.Editable = n.Job == nil && (n.Status == notes.Uploading || n.Status == notes.Review || n.Status == notes.Ready)
	if n.Status == notes.Review || n.Status == notes.Ready {
		for _, p := range n.Pages {
			if p.Transcript == nil {
				v.Untranscribed++
			}
		}
	}
	v.JobLabel = jobLabel(n, v.Worker)
	return v, nil
}

func jobLabel(n *core.Note, wk workerStatus) string {
	j := n.Job
	if j == nil {
		return ""
	}
	if j.State == "leased" {
		if j.Progress != "" {
			return "working: " + j.Progress
		}
		return "working…"
	}
	switch {
	case !wk.Online && wk.LastSeen.IsZero():
		return "waiting for the processing machine to connect."
	case !wk.Online:
		return "waiting for the processing machine (offline since " + wk.LastSeen.Local().Format("Jan 2 15:04") + ")."
	case wk.State == "gpu_unavailable" && j.Kind != notes.Speak:
		return "waiting for the GPU to be free."
	case j.Error != "":
		return "retrying after an error: " + j.Error
	}
	return "queued."
}

func (h *Web) note(w http.ResponseWriter, r *http.Request) {
	v, err := h.noteView(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "note", "layout", v)
}

// status is polled by htmx while a job runs. Once the job is gone it asks
// htmx to reload the page, which then renders the note's new state.
func (h *Web) status(w http.ResponseWriter, r *http.Request) {
	v, err := h.noteView(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Reload the whole page when the job ends, or when page previews arrived
	// since it was rendered (the thumbnails aren't part of this partial).
	if v.Note.Job == nil || r.URL.Query().Get("previews") != strconv.Itoa(v.Note.Previews()) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	h.render(w, r, http.StatusOK, "note", "job-status", v)
}

func (h *Web) settings(w http.ResponseWriter, r *http.Request) {
	v, err := h.common(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Title = "Settings"
	v.Flash = r.URL.Query().Get("msg")
	if tok := v.Settings.FeedToken; tok != "" {
		base := h.Origin + "/feed/" + tok
		v.Feeds = append(v.Feeds, feedLink{"all", "All notes", base + "/all.xml"})
		for _, c := range v.Collections {
			v.Feeds = append(v.Feeds, feedLink{strconv.FormatInt(c.ID, 10), c.Name, fmt.Sprintf("%s/c/%d.xml", base, c.ID)})
		}
	}
	h.render(w, r, http.StatusOK, "settings", "layout", v)
}

func (h *Web) saveSettings(w http.ResponseWriter, r *http.Request) {
	st, err := h.Svc.Settings(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	st.AutoApprove = r.FormValue("auto_approve") == "1"
	if v := r.FormValue("default_voice"); v != "" {
		st.DefaultVoice = v
	}
	if v := r.FormValue("default_speed"); v != "" {
		if st.DefaultSpeed, err = strconv.ParseFloat(v, 64); err != nil {
			h.fail(w, r, core.ErrInvalid)
			return
		}
	}
	if v := r.FormValue("illegible_policy"); v != "" {
		st.IllegiblePolicy = v
	}
	if err := h.Svc.SaveSettings(r.Context(), st); err != nil {
		http.Redirect(w, r, "/settings?msg="+url.QueryEscape(userMsg(err)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Web) createCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Svc.CreateCollection(r.Context(), strings.TrimSpace(r.FormValue("name"))); err != nil {
		http.Redirect(w, r, "/settings?msg="+url.QueryEscape(userMsg(err)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Web) collectionAction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("action") {
	case "rename":
		err = h.Svc.RenameCollection(r.Context(), id, strings.TrimSpace(r.FormValue("name")))
	case "delete":
		err = h.Svc.DeleteCollection(r.Context(), id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Redirect(w, r, "/settings?msg="+url.QueryEscape(userMsg(err)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// --- media -----------------------------------------------------------------------

func (h *Web) audio(w http.ResponseWriter, r *http.Request) {
	n, err := h.Svc.Get(r.Context(), r.PathValue("pid"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if n.AudioVersion == 0 {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(h.Svc.Files().Audio(n.PID, n.AudioVersion))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.URL.Query().Get("download") == "1" {
		title := n.Title
		if title == "" {
			title = "notecast"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": title + ".mp3"}))
	}
	// ServeContent does Range and HEAD, which iOS needs for audio.
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (h *Web) pageImage(w http.ResponseWriter, r *http.Request) {
	n, err := h.Svc.Get(r.Context(), r.PathValue("pid"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var page *core.Page
	for i := range n.Pages {
		if n.Pages[i].ID == id {
			page = &n.Pages[i]
		}
	}
	if page == nil {
		http.NotFound(w, r)
		return
	}
	fs := h.Svc.Files()
	path, ctype := fs.PageOrig(n.PID, id, page.Ext), contentTypeFor(page.Ext)
	switch r.PathValue("kind") {
	case "display":
		if page.HasDisplay {
			path, ctype = fs.PageDisplay(n.PID, id), "image/jpeg"
		}
	case "thumb":
		if page.HasDisplay {
			path, ctype = fs.PageThumb(n.PID, id), "image/jpeg"
		}
	case "orig":
	default:
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func contentTypeFor(ext string) string {
	switch ext {
	case "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "heic":
		return "image/heic"
	}
	return "application/octet-stream"
}

func (h *Web) static() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") || r.URL.Path == "" {
			http.NotFound(w, r) // no directory listings
			return
		}
		if h.Version == "dev" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			// URLs carry ?v=<version>, so a deploy busts the cache.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if strings.HasSuffix(r.URL.Path, ".mp3") {
			// Go's built-in MIME table lacks .mp3 (voice samples).
			w.Header().Set("Content-Type", "audio/mpeg")
		}
		files.ServeHTTP(w, r)
	}))
}

// securityHeaders applies to every public response.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; img-src 'self' data: blob:; media-src 'self'; style-src 'self'; " +
		"script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
