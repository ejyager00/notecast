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
	Svc *core.Service
	Log *slog.Logger
}

func (h *Web) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("POST /notes", h.createNote)
	mux.HandleFunc("POST /notes/{pid}/pages", h.addPage)
	mux.HandleFunc("POST /notes/{pid}/pages/{id}/{action}", h.pageAction)
	mux.HandleFunc("POST /notes/{pid}/edit", h.edit)
	mux.HandleFunc("POST /notes/{pid}/{action}", h.noteAction)
	return mux
}

func (h *Web) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, core.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, core.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		h.Log.Error("web", "path", r.URL.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
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
// transcripts arrive as transcript-<page id>.
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
		e.Speed = &f
	}
	for k, v := range r.PostForm {
		if idStr, ok := strings.CutPrefix(k, "transcript-"); ok {
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				h.fail(w, r, core.ErrInvalid)
				return
			}
			if e.Transcripts == nil {
				e.Transcripts = map[int64]string{}
			}
			e.Transcripts[id] = normalizeNewlines(v[0])
		}
	}
	if err := h.Svc.SaveEdits(r.Context(), r.PathValue("pid"), e); err != nil {
		h.fail(w, r, err)
		return
	}
	seeOther(w, r, r.PathValue("pid"))
}

// Browsers submit textareas with CRLF; store LF so edits compare equal.
func normalizeNewlines(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
