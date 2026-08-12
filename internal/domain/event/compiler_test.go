package event

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/observation"
)

func TestCompilerEmitsPersonReturnedAfterThreshold(t *testing.T) {
	compiler := NewCompiler(30 * time.Minute)
	start := time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC)

	left := personObservation("left", 1, start, false)
	returned := personObservation("returned", 2, start.Add(45*time.Minute), true)

	if got := compiler.Compile(left); len(got) != 1 || got[0].Kind != PersonLeft {
		t.Fatalf("Compile(left) = %#v, want PersonLeft", got)
	}
	got := compiler.Compile(returned)
	if len(got) != 1 || got[0].Kind != PersonReturned {
		t.Fatalf("Compile(returned) = %#v, want PersonReturned", got)
	}
	if got[0].AbsenceDuration != 45*time.Minute {
		t.Fatalf("AbsenceDuration = %s, want 45m", got[0].AbsenceDuration)
	}
}

func TestCompilerDoesNotTreatBriefDropoutAsReturn(t *testing.T) {
	compiler := NewCompiler(30 * time.Minute)
	start := time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC)
	compiler.Compile(personObservation("left", 1, start, false))

	got := compiler.Compile(personObservation("present", 2, start.Add(time.Minute), true))
	if len(got) != 1 || got[0].Kind != PersonDetected {
		t.Fatalf("Compile(brief return) = %#v, want PersonDetected", got)
	}
}

func personObservation(id string, seq uint64, at time.Time, present bool) observation.Observation {
	payload := observation.PersonPresence{Present: present}
	return observation.Observation{
		ID:             id,
		SourceID:       "test",
		SourceSeq:      seq,
		OccurredAt:     at,
		TTL:            time.Hour,
		SubjectID:      "user-1",
		Confidence:     1,
		TraceID:        "trace-1",
		PersonPresence: &payload,
	}
}
