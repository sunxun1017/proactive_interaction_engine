package supervisor

import (
	"os"
	"testing"
	"time"
)

func TestExecLauncherPassesSupervisorInstanceID(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	process, err := (ExecLauncher{}).Start(Spec{
		Command: executable, Args: []string{"-test.run=^TestExecLauncherHelperProcess$", "--"},
		Env: []string{"SUPERVISOR_HELPER_PROCESS=1"}, InstanceID: "desktop-presence-instance",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case err := <-process.Done():
		if err != nil {
			t.Fatalf("worker helper error = %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = process.Kill()
		select {
		case <-process.Done():
		case <-time.After(time.Second):
			t.Fatal("worker helper did not join after kill")
		}
		t.Fatal("worker helper exceeded its execution bound")
	}
}

func TestExecLauncherRejectsMissingInstanceID(t *testing.T) {
	if _, err := (ExecLauncher{}).Start(Spec{Command: "/bin/true"}); err == nil {
		t.Fatal("Start() error = nil, want missing instance id rejection")
	}
}

func TestExecLauncherHelperProcess(t *testing.T) {
	if os.Getenv("SUPERVISOR_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	if len(args) < 2 || args[len(args)-2] != "--instance-id" || args[len(args)-1] != "desktop-presence-instance" {
		os.Exit(2)
	}
}
