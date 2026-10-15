// Package notes is the note status machine (ARCHITECTURE §2.1), as pure
// functions. Everything that changes notes.status goes through Apply.
package notes

import "fmt"

type Status string

const (
	Uploading    Status = "uploading"
	Transcribing Status = "transcribing"
	Review       Status = "review"
	Narrating    Status = "narrating"
	Speaking     Status = "speaking"
	Ready        Status = "ready"
	Failed       Status = "failed"
)

var AllStatuses = []Status{Uploading, Transcribing, Review, Narrating, Speaking, Ready, Failed}

// Kind is a job kind; each active status corresponds to exactly one.
type Kind string

const (
	Transcribe Kind = "transcribe"
	Narrate    Kind = "narrate"
	Speak      Kind = "speak"
)

// StatusFor is the note status while a job of kind k is queued or running.
func StatusFor(k Kind) Status {
	switch k {
	case Transcribe:
		return Transcribing
	case Narrate:
		return Narrating
	case Speak:
		return Speaking
	}
	panic("unknown kind " + string(k))
}

// KindFor is the inverse of StatusFor; "" for a status with no active job.
func KindFor(s Status) Kind {
	switch s {
	case Transcribing:
		return Transcribe
	case Narrating:
		return Narrate
	case Speaking:
		return Speak
	}
	return ""
}

type Event string

const (
	Submit             Event = "submit"               // first transcription of an uploaded note
	TranscribeNew      Event = "transcribe_new"       // pages added to a reviewed/ready note
	TranscribeDone     Event = "transcribe_done"      // worker finished; wait for review
	TranscribeDoneAuto Event = "transcribe_done_auto" // worker finished; auto-approve on
	Rewrite            Event = "rewrite"              // regenerate narration from transcript
	NarrateDone        Event = "narrate_done"
	Approve            Event = "approve"
	SpeakDone          Event = "speak_done"
	Edit               Event = "edit" // transcript/narration/title saved
	Fail               Event = "fail" // job failed permanently
	Retry              Event = "retry"
	Cancel             Event = "cancel"
)

var AllEvents = []Event{Submit, TranscribeNew, TranscribeDone, TranscribeDoneAuto, Rewrite,
	NarrateDone, Approve, SpeakDone, Edit, Fail, Retry, Cancel}

// Result of a transition: the next status, and the job to enqueue (if any).
type Result struct {
	Next    Status
	Enqueue Kind
}

// ErrIllegal is returned for an event that isn't valid in the current status.
type ErrIllegal struct {
	From  Status
	Event Event
}

func (e ErrIllegal) Error() string {
	return fmt.Sprintf("can't %s a note that is %s", e.Event, e.From)
}

// Apply computes a transition. failedStage is only consulted for Retry
// (from Failed) and must be the kind of the job that failed.
func Apply(from Status, ev Event, failedStage Kind) (Result, error) {
	bad := func() (Result, error) { return Result{}, ErrIllegal{from, ev} }
	switch ev {
	case Submit:
		if from == Uploading {
			return Result{Transcribing, Transcribe}, nil
		}
	case TranscribeNew:
		if from == Review || from == Ready {
			return Result{Transcribing, Transcribe}, nil
		}
	case TranscribeDone:
		if from == Transcribing {
			return Result{Review, ""}, nil
		}
	case TranscribeDoneAuto:
		if from == Transcribing {
			return Result{Speaking, Speak}, nil
		}
	case Rewrite:
		if from == Review {
			return Result{Narrating, Narrate}, nil
		}
	case NarrateDone:
		if from == Narrating {
			return Result{Review, ""}, nil
		}
	case Approve:
		if from == Review {
			return Result{Speaking, Speak}, nil
		}
	case SpeakDone:
		if from == Speaking {
			return Result{Ready, ""}, nil
		}
	case Edit:
		// Editing a ready note sends it back to review; its audio stays live.
		if from == Review || from == Ready {
			return Result{Review, ""}, nil
		}
		if from == Uploading {
			return Result{Uploading, ""}, nil
		}
	case Fail:
		if KindFor(from) != "" {
			return Result{Failed, ""}, nil
		}
	case Retry:
		if from == Failed && failedStage != "" {
			return Result{StatusFor(failedStage), failedStage}, nil
		}
	case Cancel:
		switch from {
		case Transcribing:
			return Result{Uploading, ""}, nil
		case Narrating, Speaking:
			return Result{Review, ""}, nil
		}
	}
	return bad()
}
