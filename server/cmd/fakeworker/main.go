// Command fakeworker processes jobs with canned results, for working on the
// server and UI without the real worker or a GPU.
//
//	NOTECAST_WORKER_TOKEN=... fakeworker -url http://127.0.0.1:8791
package main

import (
	"flag"
	"log"
	"os"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/fakeworker"
	"git.ericbits.win/eric/notecast/server/internal/notes"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8791", "worker API base URL")
	delay := flag.Duration("delay", 2*time.Second, "pretend each job takes this long")
	flag.Parse()
	c := &fakeworker.Client{BaseURL: *url, Token: os.Getenv("NOTECAST_WORKER_TOKEN"), ID: "fakeworker"}
	for {
		job, err := c.Claim(notes.Transcribe, notes.Narrate, notes.Speak)
		if err != nil {
			log.Printf("claim: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if job == nil {
			continue
		}
		log.Printf("job %d: %s %s", job.JobID, job.Kind, job.NotePID)
		time.Sleep(*delay)
		res, out := fakeworker.Canned(job)
		if err := c.Complete(job, res, out); err != nil {
			log.Printf("complete: %v", err)
		}
	}
}
