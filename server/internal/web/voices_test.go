package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every voice in the picker has a sample, and no sample is left over from a
// removed voice. Fix a failure with `just voice-samples`.
func TestVoiceSamples(t *testing.T) {
	files, err := fs.Glob(staticFS, "static/voices/*.mp3")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range files {
		have[strings.TrimSuffix(strings.TrimPrefix(f, "static/voices/"), ".mp3")] = true
	}
	for _, v := range voices {
		if !have[v.ID] {
			t.Errorf("no sample for voice %s", v.ID)
		}
		delete(have, v.ID)
	}
	for id := range have {
		t.Errorf("sample for unlisted voice %s", id)
	}

	rec := httptest.NewRecorder()
	(&Web{Version: "test"}).static().ServeHTTP(rec, httptest.NewRequest("GET", "/static/voices/af_heart.mp3?v=test", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("GET sample: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
