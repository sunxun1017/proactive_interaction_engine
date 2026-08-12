package control

import (
	"testing"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestCommandValidate(t *testing.T) {
	tests := []struct {
		name    string
		command Command
		valid   bool
	}{
		{
			name: "user stop",
			command: Command{
				ID:      "control-1",
				Kind:    StopAll,
				Reason:  ReasonUserRequested,
				TraceID: "trace-1",
			},
			valid: true,
		},
		{
			name: "unknown command",
			command: Command{
				ID:      "control-2",
				Kind:    "RESTART",
				Reason:  ReasonUserRequested,
				TraceID: "trace-2",
			},
		},
		{
			name: "missing trace",
			command: Command{
				ID:     "control-3",
				Kind:   StopAll,
				Reason: ReasonShutdown,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.command.Validate()
			if test.valid && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.valid && !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Validate() error = %v, want InvalidInput", err)
			}
		})
	}
}
