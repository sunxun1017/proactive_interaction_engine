package supervisor

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
)

func TestExecLauncherPassesSupervisorInstanceID(t *testing.T) {
	t.Setenv("PROACTIVE_PROVIDER_99_ID", "parent-poison")
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	process, err := (ExecLauncher{}).Start(ProcessSpec{
		ProcessID: "camera-process", BasePermission: privacy.CameraCapture, Command: executable, Args: []string{"-test.run=^TestExecLauncherHelperProcess$", "--"},
		Env: []string{"SUPERVISOR_HELPER_PROCESS=1"}, InstanceID: "desktop-presence-instance",
		Providers: []LogicalProviderSpec{
			{ProviderID: "desktop-presence", Capability: readiness.PersonPresence},
			{ProviderID: "desktop-face-detection", Capability: readiness.FaceDetection},
		},
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
	if _, err := (ExecLauncher{}).Start(ProcessSpec{ProcessID: "camera-process", Command: "/bin/true"}); err == nil {
		t.Fatal("Start() error = nil, want missing instance id rejection")
	}
}

func TestExecLauncherRejectsOversizedInstanceID(t *testing.T) {
	if _, err := (ExecLauncher{}).Start(ProcessSpec{
		ProcessID: "camera-process", Command: "/bin/true", InstanceID: strings.Repeat("i", maxSupervisorIDBytes+1),
	}); err == nil {
		t.Fatal("Start() error = nil, want oversized instance id rejection")
	}
}

func TestExecLauncherRejectsMalformedOrReservedEnvironment(t *testing.T) {
	valid := ProcessSpec{
		ProcessID: "camera-process", BasePermission: privacy.CameraCapture, Command: "/bin/true", InstanceID: "camera-instance",
		Providers: []LogicalProviderSpec{{ProviderID: "desktop-presence", Capability: readiness.PersonPresence}},
	}
	for _, test := range []struct {
		name string
		env  []string
	}{
		{name: "missing separator", env: []string{"BROKEN"}},
		{name: "invalid name", env: []string{"1BROKEN=value"}},
		{name: "duplicate name", env: []string{"PATH=/first", "PATH=/second"}},
		{name: "nul value", env: []string{"SAFE=bad\x00value"}},
		{name: "reserved count", env: []string{"PROACTIVE_PROVIDER_COUNT=1"}},
		{name: "reserved indexed field", env: []string{"PROACTIVE_PROVIDER_0_ID=spoofed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			spec.Env = test.env
			if _, err := (ExecLauncher{}).Start(spec); err == nil {
				t.Fatal("Start() error = nil, want environment rejection")
			}
		})
	}
}

func TestExecLauncherRejectsInvalidProviderSubset(t *testing.T) {
	valid := ProcessSpec{
		ProcessID: "camera-process", BasePermission: privacy.CameraCapture, Command: "/bin/true", InstanceID: "camera-instance",
		Providers: []LogicalProviderSpec{{ProviderID: "desktop-presence", Capability: readiness.PersonPresence}},
	}
	tests := []struct {
		name   string
		mutate func(*ProcessSpec)
	}{
		{name: "empty", mutate: func(spec *ProcessSpec) { spec.Providers = nil }},
		{name: "more than four", mutate: func(spec *ProcessSpec) {
			spec.Providers = append(spec.Providers,
				LogicalProviderSpec{ProviderID: "face-detection", Capability: readiness.FaceDetection},
				LogicalProviderSpec{ProviderID: "face-identification", Capability: readiness.FaceIdentification},
				LogicalProviderSpec{ProviderID: "face-liveness", Capability: readiness.FaceLiveness},
				LogicalProviderSpec{ProviderID: "extra", Capability: readiness.FaceDetection},
			)
		}},
		{name: "wrong device capability", mutate: func(spec *ProcessSpec) { spec.Providers[0].Capability = readiness.VoiceActivity }},
		{name: "missing base capability", mutate: func(spec *ProcessSpec) { spec.Providers[0].Capability = readiness.FaceDetection }},
		{name: "duplicate provider id", mutate: func(spec *ProcessSpec) { spec.Providers = append(spec.Providers, spec.Providers[0]) }},
		{name: "duplicate capability", mutate: func(spec *ProcessSpec) {
			spec.Providers = append(spec.Providers, LogicalProviderSpec{ProviderID: "other-presence", Capability: readiness.PersonPresence})
		}},
		{name: "malformed provider id", mutate: func(spec *ProcessSpec) { spec.Providers[0].ProviderID = "bad=id" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			spec.Providers = append([]LogicalProviderSpec(nil), valid.Providers...)
			test.mutate(&spec)
			if _, err := (ExecLauncher{}).Start(spec); err == nil {
				t.Fatal("Start() error = nil, want provider subset rejection")
			}
		})
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
	want := map[string]string{
		"PROACTIVE_PROVIDER_COUNT":        "2",
		"PROACTIVE_PROVIDER_0_ID":         "desktop-face-detection",
		"PROACTIVE_PROVIDER_0_CAPABILITY": "FACE_DETECTION",
		"PROACTIVE_PROVIDER_1_ID":         "desktop-presence",
		"PROACTIVE_PROVIDER_1_CAPABILITY": "PERSON_PRESENCE",
	}
	got := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, providerEnvironmentPrefix) {
			got[name] = value
		}
	}
	if !reflect.DeepEqual(got, want) {
		os.Exit(3)
	}
}
