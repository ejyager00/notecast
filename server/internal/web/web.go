// Package web serves the public listener. Phase 2 has only the form
// endpoints that drive the note lifecycle; the HTML UI arrives in Phase 4.
package web

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"git.ericbits.win/eric/notecast/server/internal/core"
)

type Web struct {
	Svc     *core.Service
	Log     *slog.Logger
	Version string // cache-busts static assets; "dev" disables caching
	Origin  string // PUBLIC_ORIGIN, for absolute URLs in feeds

	tmpl templates
}

// Handlers returns the app (everything behind authentication) and the open
// handler (the few routes that skip it: health check, and the podcast feed
// which podcast apps fetch without a login). They are separate muxes, so no
// path trick can reach an app route through the open side.
func (h *Web) Handlers() (app, open http.Handler, err error) {
	t, err := loadTemplates()
	if err != nil {
		return nil, nil, err
	}
	h.tmpl = t

	openMux := http.NewServeMux()
	openMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	h.feedRoutes(openMux)

	mux := http.NewServeMux()
	mux.Handle("GET /static/", h.static())
	mux.HandleFunc("GET /{$}", h.library)
	mux.HandleFunc("GET /search", h.library)
	mux.HandleFunc("GET /new", h.newNote)
	mux.HandleFunc("GET /settings", h.settings)
	mux.HandleFunc("POST /settings", h.saveSettings)
	mux.HandleFunc("POST /settings/feed-token", h.rotateFeed)
	mux.HandleFunc("POST /collections", h.createCollection)
	mux.HandleFunc("POST /collections/{id}/{action}", h.collectionAction)
	mux.HandleFunc("GET /notes/{pid}", h.note)
	mux.HandleFunc("GET /notes/{pid}/status", h.status)
	mux.HandleFunc("GET /notes/{pid}/audio", h.audio)
	mux.HandleFunc("GET /notes/{pid}/pages/{id}/{kind}", h.pageImage)
	mux.HandleFunc("POST /notes", h.createNote)
	mux.HandleFunc("POST /notes/{pid}/pages", h.addPage)
	mux.HandleFunc("POST /notes/{pid}/pages/{id}/{action}", h.pageAction)
	mux.HandleFunc("POST /notes/{pid}/edit", h.edit)
	mux.HandleFunc("POST /notes/{pid}/{action}", h.noteAction)
	return securityHeaders(mux), securityHeaders(openMux), nil
}

// OpenPrefixes are the paths served by the open handler.
var OpenPrefixes = []string{"/healthz", "/feed/"}

// fail answers with an error page (or plain text for the upload XHR, which
// shows the response body next to the file).
func (h *Web) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, title := http.StatusInternalServerError, "Something went wrong"
	switch {
	case errors.Is(err, core.ErrNotFound):
		code, title = http.StatusNotFound, "Not found"
	case errors.Is(err, core.ErrInvalid):
		code, title = http.StatusBadRequest, "That didn’t work"
	case errors.Is(err, core.ErrConflict):
		code, title = http.StatusConflict, "That can’t be done right now"
	default:
		h.Log.Error("web", "path", r.URL.Path, "err", err)
	}
	msg := "Please try again."
	if code != http.StatusInternalServerError {
		msg = userMsg(err)
	}
	if strings.HasSuffix(r.URL.Path, "/pages") || h.tmpl == nil {
		http.Error(w, msg, code)
		return
	}
	back := ""
	if pid := r.PathValue("pid"); pid != "" && code != http.StatusNotFound {
		back = "/notes/" + pid
	}
	h.render(w, r, code, "error", "layout", &view{Title: title, Flash: msg, Back: back})
}

func seeOther(w http.ResponseWriter, r *http.Request, pid string) {
	http.Redirect(w, r, "/notes/"+pid, http.StatusSeeOther)
}

func optionalID(s string) (*int64, error) {
	if s == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, core.ErrInvalid
	}
	return &id, nil
}

func (h *Web) createNote(w http.ResponseWriter, r *http.Request) {
	coll, err := optionalID(r.FormValue("collection_id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	pid, err := h.Svc.CreateNote(r.Context(), coll)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	seeOther(w, r, pid)
}

// addPage takes one image per request, as the "page" field of a multipart
// form, streamed to disk without buffering it in memory.
func (h *Web) addPage(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		http.Error(w, "want multipart/form-data", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxPageBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for {
		p, err := mr.NextPart()
		if err != nil {
			http.Error(w, "no page in upload", http.StatusBadRequest)
			return
		}
		if p.FormName() != "page" {
			continue
		}
		id, err := h.Svc.AddPage(r.Context(), r.PathValue("pid"), p)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		w.Header().Set("X-Page-Id", strconv.FormatInt(id, 10))
		seeOther(w, r, r.PathValue("pid"))
		return
	}
}

func (h *Web) pageAction(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("action") {
	case "up":
		err = h.Svc.MovePage(r.Context(), pid, id, -1)
	case "down":
		err = h.Svc.MovePage(r.Context(), pid, id, 1)
	case "delete":
		err = h.Svc.DeletePage(r.Context(), pid, id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	seeOther(w, r, pid)
}

func (h *Web) noteAction(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	ctx := r.Context()
	var err error
	switch r.PathValue("action") {
	case "submit":
		err = h.Svc.Submit(ctx, pid)
	case "transcribe-new":
		err = h.Svc.TranscribeNew(ctx, pid)
	case "rewrite":
		err = h.Svc.Rewrite(ctx, pid)
	case "approve":
		err = h.Svc.Approve(ctx, pid, r.FormValue("force") == "1")
	case "retry":
		err = h.Svc.Retry(ctx, pid)
	case "cancel":
		err = h.Svc.Cancel(ctx, pid)
	case "delete":
		if err = h.Svc.Delete(ctx, pid); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	seeOther(w, r, pid)
}

// edit saves the review form. Fields absent from the form are unchanged;
// transcripts and narrations arrive as transcript-<page id> and
// narration-<page id>, and seen-<field> carries the fingerprint of what the
// form showed (core.Edits.Seen).
func (h *Web) edit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var e core.Edits
	if v, ok := r.PostForm["title"]; ok {
		t := strings.TrimSpace(v[0])
		e.Title = &t
	}
	if v, ok := r.PostForm["narration"]; ok {
		n := normalizeNewlines(v[0])
		e.Narration = &n
	}
	if v, ok := r.PostForm["collection_id"]; ok {
		id, err := optionalID(v[0])
		if err != nil {
			h.fail(w, r, err)
			return
		}
		e.CollectionID = &id
	}
	if v, ok := r.PostForm["voice"]; ok {
		e.Voice = &v[0]
	}
	if v, ok := r.PostForm["speed"]; ok && v[0] != "" {
		f, err := strconv.ParseFloat(v[0], 64)
		if err != nil {
			h.fail(w, r, core.ErrInvalid)
			return
		}
		e.Speed = &f // 0 = use the default
	}
	for k, v := range r.PostForm {
		if field, ok := strings.CutPrefix(k, "seen-"); ok {
			if e.Seen == nil {
				e.Seen = map[string]string{}
			}
			e.Seen[field] = v[0]
			continue
		}
		for prefix, dst := range map[string]*map[int64]string{
			"transcript-": &e.Transcripts, "narration-": &e.Narrations,
		} {
			idStr, ok := strings.CutPrefix(k, prefix)
			if !ok {
				continue
			}
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				h.fail(w, r, core.ErrInvalid)
				return
			}
			if *dst == nil {
				*dst = map[int64]string{}
			}
			(*dst)[id] = normalizeNewlines(v[0])
		}
	}
	pid := r.PathValue("pid")
	if err := h.Svc.SaveEdits(r.Context(), pid, e); err != nil {
		if errors.Is(err, core.ErrConflict) || errors.Is(err, core.ErrInvalid) {
			backWithMessage(w, r, pid, userMsg(err))
			return
		}
		h.fail(w, r, err)
		return
	}
	// The review form's buttons save first and then act, so pressing
	// "Make audio" never discards edits made on the way.
	var err error
	switch r.PostForm.Get("then") {
	case "approve":
		err = h.Svc.Approve(r.Context(), pid, false)
	case "approve-anyway":
		err = h.Svc.Approve(r.Context(), pid, true)
	case "rewrite":
		err = h.Svc.Rewrite(r.Context(), pid)
	}
	if err != nil {
		if errors.Is(err, core.ErrConflict) || errors.Is(err, core.ErrInvalid) {
			backWithMessage(w, r, pid, "Saved. "+userMsg(err))
			return
		}
		h.fail(w, r, err)
		return
	}
	seeOther(w, r, pid)
}

// Browsers submit textareas with CRLF; store LF so edits compare equal.
func normalizeNewlines(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
