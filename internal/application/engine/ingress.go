package engine

import (
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
)

// ingress performs validation, deduplication, and monotonic source sequencing.
// It is owned by one Engine and is intentionally not concurrency-safe.
type ingress struct {
	seenIDs map[string]struct{}
	lastSeq map[string]uint64
}

func newIngress() *ingress {
	return &ingress{
		seenIDs: make(map[string]struct{}),
		lastSeq: make(map[string]uint64),
	}
}

func (i *ingress) accept(input observation.Observation, now time.Time) error {
	if err := input.ValidateAt(now); err != nil {
		return err
	}
	if _, exists := i.seenIDs[input.ID]; exists {
		return fault.New(fault.StaleInput, "accept observation", fmt.Errorf("duplicate observation id %s", input.ID))
	}
	if last, exists := i.lastSeq[input.SourceID]; exists && input.SourceSeq <= last {
		return fault.New(
			fault.StaleInput,
			"accept observation",
			fmt.Errorf("source %s sequence %d is not greater than %d", input.SourceID, input.SourceSeq, last),
		)
	}
	i.seenIDs[input.ID] = struct{}{}
	i.lastSeq[input.SourceID] = input.SourceSeq
	return nil
}
