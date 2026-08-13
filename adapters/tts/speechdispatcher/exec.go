package speechdispatcher

import (
	"context"
	"os/exec"
)

// ExecRunner invokes a binary directly through the operating system.
type ExecRunner struct{}

// Run executes binary with args and waits for it to exit.
func (ExecRunner) Run(ctx context.Context, binary string, args ...string) error {
	return exec.CommandContext(ctx, binary, args...).Run()
}
