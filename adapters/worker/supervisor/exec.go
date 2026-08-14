package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// ExecLauncher starts an explicit command in its own process group.
type ExecLauncher struct{}

func (ExecLauncher) Start(spec Spec) (Process, error) {
	if strings.TrimSpace(spec.InstanceID) == "" || spec.InstanceID != strings.TrimSpace(spec.InstanceID) {
		return nil, errors.New("worker instance id is required")
	}
	args := append(append([]string(nil), spec.Args...), "--instance-id", spec.InstanceID)
	command := exec.Command(spec.Command, args...)
	command.Dir = spec.WorkingDir
	command.Env = append(os.Environ(), spec.Env...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &execProcess{process: command.Process, done: make(chan error, 1)}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	return process, nil
}

type execProcess struct {
	process *os.Process
	done    chan error
}

func (p *execProcess) Signal(signal os.Signal) error {
	value, ok := signal.(syscall.Signal)
	if !ok {
		return errors.New("process signal must be a syscall signal")
	}
	return syscall.Kill(-p.process.Pid, value)
}

func (p *execProcess) Kill() error {
	return syscall.Kill(-p.process.Pid, syscall.SIGKILL)
}

func (p *execProcess) Done() <-chan error { return p.done }
