// Package fakeworker is a worker-API client that returns canned results.
// It drives the integration tests and `cmd/fakeworker` (for UI work without
// the GPU), and is the reference for the protocol the Python worker speaks.
package fakeworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8791
	Token   string
	ID      string
	HTTP    *http.Client
}

func (c *Client) do(method, path, lease string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Worker-Id", c.ID)
	req.Header.Set("X-Worker-Version", "fake")
	req.Header.Set("X-Worker-State", "idle")
	if lease != "" {
		req.Header.Set("X-Lease-Token", lease)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// StatusError is a non-2xx response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, strings.TrimSpace(e.Body))
}

func check(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &StatusError{resp.StatusCode, string(b)}
	}
	return resp, nil
}

// Claim returns nil when the long-poll ends with no job.
func (c *Client) Claim(kinds ...notes.Kind) (*core.Claimed, error) {
	b, _ := json.Marshal(map[string]any{"kinds": kinds})
	resp, err := check(c.do("POST", "/worker/v1/claim", "", bytes.NewReader(b), "application/json"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var cl core.Claimed
	return &cl, json.NewDecoder(resp.Body).Decode(&cl)
}

func (c *Client) Input(job *core.Claimed, name string) ([]byte, error) {
	resp, err := check(c.do("GET", fmt.Sprintf("/worker/v1/jobs/%d/inputs/%s", job.JobID, name), job.LeaseToken, nil, ""))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *Client) Heartbeat(job *core.Claimed, progress string) error {
	b, _ := json.Marshal(map[string]string{"progress": progress})
	resp, err := check(c.do("POST", fmt.Sprintf("/worker/v1/jobs/%d/heartbeat", job.JobID), job.LeaseToken, bytes.NewReader(b), "application/json"))
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (c *Client) Fail(job *core.Claimed, msg string, retryable bool) error {
	b, _ := json.Marshal(map[string]any{"error": msg, "retryable": retryable})
	resp, err := check(c.do("POST", fmt.Sprintf("/worker/v1/jobs/%d/fail", job.JobID), job.LeaseToken, bytes.NewReader(b), "application/json"))
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (c *Client) Complete(job *core.Claimed, res core.Result, out map[string][]byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	rj, _ := json.Marshal(res)
	fw, _ := mw.CreateFormField("result")
	fw.Write(rj)
	for name, data := range out {
		fw, _ := mw.CreateFormFile(name, name)
		fw.Write(data)
	}
	mw.Close()
	resp, err := check(c.do("POST", fmt.Sprintf("/worker/v1/jobs/%d/complete", job.JobID), job.LeaseToken, &buf, mw.FormDataContentType()))
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Previews uploads display/thumb JPEGs for a transcribe job's pages early.
func (c *Client) Previews(job *core.Claimed, out map[string][]byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range out {
		fw, _ := mw.CreateFormFile(name, name)
		fw.Write(data)
	}
	mw.Close()
	resp, err := check(c.do("POST", fmt.Sprintf("/worker/v1/jobs/%d/previews", job.JobID), job.LeaseToken, &buf, mw.FormDataContentType()))
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Canned builds a plausible result for a claimed job.
func Canned(job *core.Claimed) (core.Result, map[string][]byte) {
	out := map[string][]byte{}
	prov := map[string]any{"provider": "fake", "model": "fake", "prompt": "none"}
	switch job.Kind {
	case notes.Transcribe:
		var res core.Result
		for _, id := range job.Transcribe {
			res.Pages = append(res.Pages, core.PageResult{PageID: id,
				Transcript: fmt.Sprintf("# Glycolysis\n- page %d: glucose -> 2 pyruvate\n- net 2 ATP", id)})
			out[fmt.Sprintf("display-%d.jpg", id)] = TinyJPEG()
			out[fmt.Sprintf("thumb-%d.jpg", id)] = TinyJPEG()
		}
		res.Segments = segments(job, "Glucose is split into two molecules of pyruvate, netting two ATP.")
		if job.Meta {
			res.Meta = &core.MetaResult{Title: "Glycolysis basics", Summary: "How glucose becomes pyruvate.",
				Tags: []string{"metabolism"}}
		}
		res.Provenance = prov
		return res, out
	case notes.Narrate:
		res := core.Result{Segments: segments(job, "Revised narration mentioning mitochondria."), Provenance: prov}
		if job.Meta {
			res.Meta = &core.MetaResult{Title: "Glycolysis, revised", Summary: "Revised.",
				Tags: []string{"metabolism", "revised"}}
		}
		return res, out
	case notes.Speak:
		out["audio.mp3"] = append([]byte("ID3"), make([]byte, 1024)...)
		out["chapters.json"] = []byte(`{"version":"1.2.0","chapters":[{"startTime":0,"title":"Glycolysis"}]}`)
		return core.Result{Audio: &core.AudioResult{Seconds: 12.5, Voice: job.Voice, Speed: job.Speed,
			Chapters: []core.Chapter{{Start: 0, Title: "Glycolysis"}}}, Provenance: prov}, out
	}
	return core.Result{}, out
}

// segments narrates each page the claim marked.
func segments(job *core.Claimed, text string) []core.SegmentResult {
	var out []core.SegmentResult
	for _, p := range job.Pages {
		if p.Narrate {
			out = append(out, core.SegmentResult{PageID: p.PageID,
				Narration: fmt.Sprintf("## Page %d\n%s", p.Position, text)})
		}
	}
	return out
}

// Process claims and completes one job; false if there was none.
func (c *Client) Process(kinds ...notes.Kind) (bool, error) {
	job, err := c.Claim(kinds...)
	if err != nil || job == nil {
		return false, err
	}
	for _, p := range job.Pages {
		if p.Input != "" {
			if _, err := c.Input(job, p.Input); err != nil {
				return true, err
			}
		}
	}
	res, out := Canned(job)
	return true, c.Complete(job, res, out)
}

// TinyJPEG is a valid 8x8 JPEG.
func TinyJPEG() []byte {
	img := image.NewGray(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = color.Gray{Y: 200}.Y
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}
