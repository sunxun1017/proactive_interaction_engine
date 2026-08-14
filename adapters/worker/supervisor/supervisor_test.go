package supervisor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
	"proactive-interaction-engine/internal/runtime/provider"
)

func TestSupervisorStartsOnlyAfterDurablePermissionAndTracksHealthyLease(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan Spec, 2)}
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	requireProviderState(t, supervisor, "desktop-presence", provider.Disabled, provider.ReasonDisabledByUser)
	select {
	case spec := <-launcher.starts:
		t.Fatalf("worker started without permission: %#v", spec)
	default:
	}

	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true}); err != nil {
		t.Fatalf("Change() error = %v", err)
	}
	spec := receiveStart(t, launcher.starts)
	if spec.ProviderID != "desktop-presence" || spec.Permission != privacy.CameraCapture {
		t.Fatalf("started spec = %#v", spec)
	}
	requireProviderState(t, supervisor, "desktop-presence", provider.Starting, provider.ReasonNone)

	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: "camera-1", ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Minute),
	}})
	requireProviderState(t, supervisor, "desktop-presence", provider.Running, provider.ReasonNone)

	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture}); err != nil {
		t.Fatalf("disable Change() error = %v", err)
	}
	requireProviderState(t, supervisor, "desktop-presence", provider.Disabled, provider.ReasonDisabledByUser)
	process := launcher.process(0)
	if got := process.signals(); !reflect.DeepEqual(got, []os.Signal{syscall.SIGTERM}) {
		t.Fatalf("process signals = %#v, want SIGTERM", got)
	}
}

func TestSupervisorCrashDegradesWithoutRestartUntilExplicitRetry(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.MicrophoneCapture, Enabled: true}); err != nil {
		t.Fatalf("seed microphone permission: %v", err)
	}
	launcher := &fakeLauncher{starts: make(chan Spec, 3)}
	supervisor := newTestSupervisor(t, permissions, newFakeLeases(), launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	receiveStart(t, launcher.starts)
	launcher.process(0).exit(errors.New("capture failed"))
	requireProviderState(t, supervisor, "desktop-vad", provider.Degraded, provider.ReasonInternalError)
	select {
	case spec := <-launcher.starts:
		t.Fatalf("crashed worker restarted automatically: %#v", spec)
	default:
	}

	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.MicrophoneCapture}); err != nil {
		t.Fatalf("disable microphone: %v", err)
	}
	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.MicrophoneCapture, Enabled: true}); err != nil {
		t.Fatalf("retry microphone: %v", err)
	}
	receiveStart(t, launcher.starts)
}

func TestSupervisorShutdownStopsAndJoinsEveryStartedWorker(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.MicrophoneCapture} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("seed %s permission: %v", permission, err)
		}
	}
	launcher := &fakeLauncher{starts: make(chan Spec, 3)}
	supervisor := newTestSupervisor(t, permissions, newFakeLeases(), launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	receiveStart(t, launcher.starts)
	receiveStart(t, launcher.starts)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not join workers")
	}
	for index := 0; index < 2; index++ {
		process := launcher.process(index)
		if got := process.signals(); !reflect.DeepEqual(got, []os.Signal{syscall.SIGTERM}) {
			t.Fatalf("process %d signals = %#v", index, got)
		}
		select {
		case <-process.Done():
		default:
			t.Fatalf("process %d was not joined", index)
		}
	}
}

func TestSupervisorHealthCheckDegradesAtLeaseDeadline(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true}); err != nil {
		t.Fatalf("seed camera permission: %v", err)
	}
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan Spec, 2)}
	supervisor, err := New(Config{StopTimeout: time.Second, HealthCheckInterval: time.Second}, permissions, leases, launcher, clock, []Spec{
		{ProviderID: "desktop-presence", Permission: privacy.CameraCapture, Command: "/python"},
		{ProviderID: "desktop-vad", Permission: privacy.MicrophoneCapture, Command: "/python"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	receiveStart(t, launcher.starts)
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: "camera-1", ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Second),
	}})
	requireProviderState(t, supervisor, "desktop-presence", provider.Running, provider.ReasonNone)

	clock.Advance(time.Second)
	requireProviderState(t, supervisor, "desktop-presence", provider.Degraded, provider.ReasonDeviceUnavailable)
}

func newTestSupervisor(t *testing.T, permissions *privacy.Service, leases *fakeLeases, launcher *fakeLauncher, now time.Time) *Supervisor {
	t.Helper()
	supervisor, err := New(Config{StopTimeout: time.Second, HealthCheckInterval: time.Second}, permissions, leases, launcher, engineclock.NewFake(now), []Spec{
		{ProviderID: "desktop-presence", Permission: privacy.CameraCapture, Command: "/python", Args: []string{"camera.py"}},
		{ProviderID: "desktop-vad", Permission: privacy.MicrophoneCapture, Command: "/python", Args: []string{"microphone.py"}},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return supervisor
}

func newPermissionService(t *testing.T, now time.Time) *privacy.Service {
	t.Helper()
	service, err := privacy.New(context.Background(), &memoryPermissions{}, engineclock.NewFake(now))
	if err != nil {
		t.Fatalf("privacy.New() error = %v", err)
	}
	return service
}

func requireProviderState(t *testing.T, source *Supervisor, id string, state provider.State, reason provider.Reason) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		snapshot := source.CurrentProviderRuntime()
		for _, runtime := range snapshot.Providers {
			if runtime.ProviderID == id && runtime.State == state && runtime.Reason == reason {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("provider %q state = %#v, want %s/%s", id, snapshot, state, reason)
		default:
		}
	}
}

func receiveStart(t *testing.T, starts <-chan Spec) Spec {
	t.Helper()
	select {
	case spec := <-starts:
		return spec
	case <-time.After(time.Second):
		t.Fatal("worker was not started")
		return Spec{}
	}
}

type memoryPermissions struct{ snapshot privacy.Snapshot }

func (m *memoryPermissions) Load(context.Context) (privacy.Snapshot, error) { return m.snapshot, nil }
func (m *memoryPermissions) Save(_ context.Context, expected uint64, snapshot privacy.Snapshot) error {
	if m.snapshot.Revision != expected {
		return errors.New("revision conflict")
	}
	m.snapshot = snapshot
	return nil
}

type fakeLeases struct {
	mu      sync.Mutex
	current []readiness.ProviderSnapshot
	updates chan []readiness.ProviderSnapshot
}

func newFakeLeases() *fakeLeases {
	return &fakeLeases{updates: make(chan []readiness.ProviderSnapshot, 8)}
}
func (f *fakeLeases) Snapshots() []readiness.ProviderSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneLeaseSnapshots(f.current)
}
func (f *fakeLeases) Subscribe() ([]readiness.ProviderSnapshot, <-chan []readiness.ProviderSnapshot, func()) {
	return f.Snapshots(), f.updates, func() {}
}
func (f *fakeLeases) publish(snapshots []readiness.ProviderSnapshot) {
	f.mu.Lock()
	f.current = cloneLeaseSnapshots(snapshots)
	f.mu.Unlock()
	f.updates <- cloneLeaseSnapshots(snapshots)
}

type fakeLauncher struct {
	mu        sync.Mutex
	starts    chan Spec
	processes []*fakeProcess
}

func (f *fakeLauncher) Start(spec Spec) (Process, error) {
	process := &fakeProcess{done: make(chan error, 1)}
	f.mu.Lock()
	f.processes = append(f.processes, process)
	f.mu.Unlock()
	f.starts <- spec
	return process, nil
}
func (f *fakeLauncher) process(index int) *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.processes[index]
}

type fakeProcess struct {
	mu          sync.Mutex
	done        chan error
	sent        bool
	signalsSeen []os.Signal
}

func (f *fakeProcess) Signal(signal os.Signal) error {
	f.mu.Lock()
	f.signalsSeen = append(f.signalsSeen, signal)
	f.mu.Unlock()
	f.exit(nil)
	return nil
}
func (f *fakeProcess) Kill() error        { f.exit(nil); return nil }
func (f *fakeProcess) Done() <-chan error { return f.done }
func (f *fakeProcess) exit(err error) {
	f.mu.Lock()
	if !f.sent {
		f.sent = true
		f.done <- err
		close(f.done)
	}
	f.mu.Unlock()
}
func (f *fakeProcess) signals() []os.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]os.Signal(nil), f.signalsSeen...)
}

func cloneLeaseSnapshots(input []readiness.ProviderSnapshot) []readiness.ProviderSnapshot {
	output := append([]readiness.ProviderSnapshot(nil), input...)
	for index := range output {
		output[index].Capabilities = append([]readiness.CapabilityKind(nil), output[index].Capabilities...)
	}
	return output
}
