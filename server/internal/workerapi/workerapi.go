// Package workerapi serves /worker/v1 on the worker listener (ARCHITECTURE
// §4.6). It is never routed through Caddy.
//
// Every request carries:
//
//	Authorization: Bearer <worker token>
//	X-Worker-Id, X-Worker-Version, X-Worker-State, X-Worker-Detail
//
// and job-scoped requests also carry X-Lease-Token.
package workerapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

type API struct {
	Svc         *core.Service
	TokenSHA256 []byte
	Wait        time.Duration // long-poll; default 25 s
	Log         *slog.Logger
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.Handle("POST /worker/v1/claim", a.authed(a.claim))
	mux.Handle("GET /worker/v1/jobs/{id}/inputs/{name}", a.authed(a.input))
	mux.Handle("POST /worker/v1/jobs/{id}/heartbeat", a.authed(a.heartbeat))
	mux.Handle("POST /worker/v1/jobs/{id}/complete", a.authed(a.complete))
	mux.Handle("POST /worker/v1/jobs/{id}/fail", a.authed(a.fail))
	return mux
}

func (a *API) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(tok))
		if !ok || subtle.ConstantTimeCompare(sum[:], a.TokenSHA256) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func info(r *http.Request) core.WorkerInfo {
	return core.WorkerInfo{
		ID:      r.Header.Get("X-Worker-Id"),
		Version: r.Header.Get("X-Worker-Version"),
		State:   r.Header.Get("X-Worker-State"),
		Detail:  r.Header.Get("X-Worker-Detail"),
	}
}

func (a *API) fail500(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrLeaseLost):
		http.Error(w, "lease lost", http.StatusConflict)
	case errors.Is(err, core.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, core.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, core.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		a.Log.Error("worker api", "path", r.URL.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func jobID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func (a *API) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kinds []notes.Kind `json:"kinds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	wait := a.Wait
	if wait == 0 {
		wait = 25 * time.Second
	}
	c, err := a.Svc.Claim(r.Context(), core.ClaimRequest{WorkerInfo: info(r), Kinds: body.Kinds}, wait)
	if err != nil {
		a.fail500(w, r, err)
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c)
}

func (a *API) input(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path, ctype, err := a.Svc.Input(r.Context(), id, r.Header.Get("X-Lease-Token"), r.PathValue("name"))
	if err != nil {
		a.fail500(w, r, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		a.fail500(w, r, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", ctype)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Progress string `json:"progress"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if err := a.Svc.Heartbeat(r.Context(), id, r.Header.Get("X-Lease-Token"), body.Progress, info(r)); err != nil {
		a.fail500(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) fail(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := a.Svc.FailJob(r.Context(), id, r.Header.Get("X-Lease-Token"), body.Error, body.Retryable, info(r)); err != nil {
		a.fail500(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var outputName = regexp.MustCompile(`^(?:(?:display|thumb)-\d+\.jpg|audio\.mp3|chapters\.json)$`)

func limitFor(name string) int64 {
	switch {
	case strings.HasSuffix(name, ".jpg"):
		return core.MaxImageResult
	case strings.HasSuffix(name, ".mp3"):
		return core.MaxAudioResult
	}
	return 1 << 20
}

// complete takes multipart/form-data: a "result" part (result.json) plus one
// part per output file, each named by its file name.
func (a *API) complete(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		http.Error(w, "want multipart/form-data", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxAudioResult+64<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var res *core.Result
	out := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		name := p.FormName()
		if name == "result" {
			res = new(core.Result)
			if err := json.NewDecoder(io.LimitReader(p, 4<<20)).Decode(res); err != nil {
				http.Error(w, "bad result json", http.StatusBadRequest)
				return
			}
			continue
		}
		if !outputName.MatchString(name) {
			http.Error(w, fmt.Sprintf("unexpected part %q", name), http.StatusBadRequest)
			return
		}
		limit := limitFor(name)
		b, err := io.ReadAll(io.LimitReader(p, limit+1))
		if err != nil {
			http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(b)) > limit {
			http.Error(w, name+" too large", http.StatusRequestEntityTooLarge)
			return
		}
		out[name] = b
	}
	if res == nil {
		http.Error(w, "missing result part", http.StatusBadRequest)
		return
	}
	if err := a.Svc.Complete(r.Context(), id, r.Header.Get("X-Lease-Token"), *res, out, info(r)); err != nil {
		a.fail500(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
