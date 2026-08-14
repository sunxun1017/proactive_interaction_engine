package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ExecLauncher starts an explicit command in its own process group.
type ExecLauncher struct{}

func (ExecLauncher) Start(spec Spec) (Process, error) {
	command := exec.Command(spec.Command, spec.Args...)
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
