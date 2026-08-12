package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/fault"
)

// Driver returns a complete, ordered lifecycle synchronously.
type Driver struct {
	mu           sync.Mutex
	capabilities behavior.Capabilities
	clock        port.Clock
	executed     map[string][]behavior.ActionStatus
	commands     []behavior.ActionCommand
	stopped      bool
}

func NewDriver(capabilities behavior.Capabilities, clock port.Clock) *Driver {
	copyOfCapabilities := make(behavior.Capabilities, len(capabilities))
	for action, capability := range capabilities {
		copyOfCapabilities[action] = capability
	}
	return &Driver{
		capabilities: copyOfCapabilities,
		clock:        clock,
		executed:     make(map[string][]behavior.ActionStatus),
	}
}

func (d *Driver) Capabilities(ctx context.Context) (behavior.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	output := make(behavior.Capabilities, len(d.capabilities))
	for action, capability := range d.capabilities {
		output[action] = capability
	}
	return output, nil
}

func (d *Driver) Execute(ctx context.Context, command behavior.ActionCommand) (<-chan behavior.ActionStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return nil, fault.New(fault.AdapterRejected, "fake execute", errors.New("adapter is stopped"))
	}
	if !d.capabilities[command.RequiredCapability].Supported {
		return nil, fault.New(
			fault.CapabilityMissing,
			"fake execute",
			fmt.Errorf("capability %s is unsupported", command.RequiredCapability),
		)
	}
	if previous, exists := d.executed[command.ID]; exists {
		return statusStream(previous), nil
	}

	now := d.clock.Now()
	if now.After(command.Deadline) {
		return nil, fault.New(fault.DeadlineExceeded, "fake execute", fmt.Errorf("action %s deadline elapsed", command.ID))
	}
	states := []behavior.ActionState{
		behavior.ActionDispatched,
		behavior.ActionAccepted,
		behavior.ActionStarted,
		behavior.ActionCompleted,
	}
	statuses := make([]behavior.ActionStatus, 0, len(states))
	for _, state := range states {
		statuses = append(statuses, behavior.ActionStatus{
			ActionID:      command.ID,
			InteractionID: command.InteractionID,
			TraceID:       command.TraceID,
			State:         state,
			OccurredAt:    now,
		})
	}
	d.executed[command.ID] = statuses
	d.commands = append(d.commands, command)
	return statusStream(statuses), nil
}

func statusStream(statuses []behavior.ActionStatus) <-chan behavior.ActionStatus {
	stream := make(chan behavior.ActionStatus, len(statuses))
	for _, status := range statuses {
		stream <- status
	}
	close(stream)
	return stream
}

func (d *Driver) StopAll(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	return nil
}

// Commands returns a copy of commands accepted for first-time execution.
func (d *Driver) Commands() []behavior.ActionCommand {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]behavior.ActionCommand(nil), d.commands...)
}
