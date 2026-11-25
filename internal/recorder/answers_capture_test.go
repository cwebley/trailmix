package recorder

import (
	"strings"
	"testing"

	"github.com/cwebley/trailmix/internal/transcription"
)

func TestQuestionPromptIncludesCaptureFailureWithCompleteTranscription(t *testing.T) {
	seconds := 565.8
	prompt, err := questionPrompt(askInput{Question: "What happened in the orientation?", Notes: ptr("rough notes")}, transcriptSnapshot{
		MeetingID:     "0123456789abcdef0123456789abcdef",
		Transcription: transcription.Status{State: "complete"},
		Capture:       &captureHealth{State: "failed", Error: "No display available", LastCapturedSeconds: &seconds},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"transcription_state":"complete"`, `"capture":`, `"state":"failed"`, `"last_captured_seconds":565.8`, "No display available", "Complete transcription only covers saved audio"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
