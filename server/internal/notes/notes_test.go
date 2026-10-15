package notes

import (
	"errors"
	"testing"
)

// Every (status, event) pair, so adding a status or event without deciding
// its transitions fails here.
func TestApplyExhaustive(t *testing.T) {
	type key struct {
		s Status
		e Event
	}
	legal := map[key]Result{
		{Uploading, Submit}:                {Transcribing, Transcribe},
		{Review, TranscribeNew}:            {Transcribing, Transcribe},
		{Ready, TranscribeNew}:             {Transcribing, Transcribe},
		{Transcribing, TranscribeDone}:     {Review, ""},
		{Transcribing, TranscribeDoneAuto}: {Speaking, Speak},
		{Review, Rewrite}:                  {Narrating, Narrate},
		{Narrating, NarrateDone}:           {Review, ""},
		{Review, Approve}:                  {Speaking, Speak},
		{Speaking, SpeakDone}:              {Ready, ""},
		{Uploading, Edit}:                  {Uploading, ""},
		{Review, Edit}:                     {Review, ""},
		{Ready, Edit}:                      {Review, ""},
		{Transcribing, Fail}:               {Failed, ""},
		{Narrating, Fail}:                  {Failed, ""},
		{Speaking, Fail}:                   {Failed, ""},
		{Failed, Retry}:                    {Narrating, Narrate}, // with failedStage=Narrate
		{Transcribing, Cancel}:             {Uploading, ""},
		{Narrating, Cancel}:                {Review, ""},
		{Speaking, Cancel}:                 {Review, ""},
	}
	for _, s := range AllStatuses {
		for _, e := range AllEvents {
			got, err := Apply(s, e, Narrate)
			want, ok := legal[key{s, e}]
			if !ok {
				var ill ErrIllegal
				if !errors.As(err, &ill) {
					t.Errorf("%s + %s: want illegal, got %+v %v", s, e, got, err)
				}
				continue
			}
			if err != nil || got != want {
				t.Errorf("%s + %s = %+v, %v; want %+v", s, e, got, err, want)
			}
		}
	}
}

func TestRetryNeedsStage(t *testing.T) {
	if _, err := Apply(Failed, Retry, ""); err == nil {
		t.Fatal("retry without a failed stage should be illegal")
	}
	for _, k := range []Kind{Transcribe, Narrate, Speak} {
		r, err := Apply(Failed, Retry, k)
		if err != nil || r.Enqueue != k || r.Next != StatusFor(k) || KindFor(r.Next) != k {
			t.Errorf("retry %s: %+v %v", k, r, err)
		}
	}
}
