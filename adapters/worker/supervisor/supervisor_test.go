package supervisor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
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
	if spec.InstanceID != "desktop-presence-instance-1" {
		t.Fatalf("instance id = %q", spec.InstanceID)
	}
	requireProviderState(t, supervisor, "desktop-presence", provider.Starting, provider.ReasonNone)

	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: spec.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
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
	leases := newFakeLeases()
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	receiveStart(t, launcher.starts)
	launcher.process(0).exit(errors.New("capture failed"))
	requireProviderState(t, supervisor, "desktop-vad", provider.Degraded, provider.ReasonInternalError)
	if got := leases.revokedSnapshot(); !reflect.DeepEqual(got, []string{"desktop-vad"}) {
		t.Fatalf("revoked providers = %#v, want crashed desktop-vad", got)
	}
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
	config := testConfig()
	supervisor, err := New(config, permissions, leases, launcher, clock, []Spec{
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
	spec := receiveStart(t, launcher.starts)
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: spec.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Second),
	}})
	requireProviderState(t, supervisor, "desktop-presence", provider.Running, provider.ReasonNone)

	clock.Advance(time.Second)
	requireProviderState(t, supervisor, "desktop-presence", provider.Degraded, provider.ReasonDeviceUnavailable)
}

func TestSupervisorIgnoresLeaseFromPreviousWorkerInstance(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true}); err != nil {
		t.Fatalf("seed camera permission: %v", err)
	}
	leases := newFakeLeases()
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: "desktop-presence-old", ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Minute),
	}})
	launcher := &fakeLauncher{starts: make(chan Spec, 2)}
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	spec := receiveStart(t, launcher.starts)
	requireProviderState(t, supervisor, "desktop-presence", provider.Starting, provider.ReasonNone)
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: spec.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Minute),
	}})
	requireProviderState(t, supervisor, "desktop-presence", provider.Running, provider.ReasonNone)
}

func TestSupervisorRestartDoesNotInheritPreviousInstanceHealth(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true}); err != nil {
		t.Fatalf("seed camera permission: %v", err)
	}
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan Spec, 3)}
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := receiveStart(t, launcher.starts)
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: first.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Minute),
	}})
	requireProviderState(t, supervisor, "desktop-presence", provider.Running, provider.ReasonNone)
	launcher.process(0).exit(errors.New("camera failed"))
	requireProviderState(t, supervisor, "desktop-presence", provider.Degraded, provider.ReasonInternalError)

	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture}); err != nil {
		t.Fatalf("disable camera: %v", err)
	}
	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true}); err != nil {
		t.Fatalf("retry camera: %v", err)
	}
	second := receiveStart(t, launcher.starts)
	if second.InstanceID == first.InstanceID {
		t.Fatalf("restarted instance id = %q, want a fresh value", second.InstanceID)
	}
	requireProviderState(t, supervisor, "desktop-presence", provider.Starting, provider.ReasonNone)
}

func TestSupervisorReconcilesPersistedPermissionInsteadOfChangeReturnOrder(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newReorderingPermissions()
	supervisor, err := New(testConfig(), permissions, newFakeLeases(), &fakeLauncher{starts: make(chan Spec, 2)}, engineclock.NewFake(now), testSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	enableDone := make(chan error, 1)
	go func() {
		_, changeErr := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true})
		enableDone <- changeErr
	}()
	<-permissions.enablePersisted
	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture}); err != nil {
		t.Fatalf("disable Change() error = %v", err)
	}
	close(permissions.releaseEnable)
	if err := <-enableDone; err != nil {
		t.Fatalf("enable Change() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	for index := 0; index < 2; index++ {
		select {
		case <-permissions.currentCalls:
		case <-time.After(time.Second):
			t.Fatal("supervisor did not reconcile latest persisted permissions")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case spec := <-supervisor.launcher.(*fakeLauncher).starts:
		t.Fatalf("worker started from stale enable completion: %#v", spec)
	default:
	}
}

func TestSupervisorChangeReturnsPersistedSnapshotAfterContextCancellation(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	permissions := &cancelingPermissions{cancel: cancel, snapshot: permissionSnapshot(1, true)}
	supervisor, err := New(testConfig(), permissions, newFakeLeases(), &fakeLauncher{starts: make(chan Spec, 2)}, engineclock.NewFake(now), testSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	supervisor.reconcile <- struct{}{}

	snapshot, err := supervisor.Change(ctx, privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true})
	if err != nil {
		t.Fatalf("Change() error after durable save = %v", err)
	}
	if snapshot.Revision != 1 || !permissionEnabled(snapshot, privacy.CameraCapture) {
		t.Fatalf("Change() snapshot = %#v", snapshot)
	}
}

func TestSupervisorStopProcessIsBoundedAfterKillFailure(t *testing.T) {
	closedTimer := make(chan time.Time)
	close(closedTimer)
	supervisor := &Supervisor{config: Config{StopTimeout: time.Second, after: func(time.Duration) <-chan time.Time { return closedTimer }}}
	process := &fakeProcess{done: make(chan error), hangOnSignal: true, killErr: errors.New("kill denied")}

	done := make(chan error, 1)
	go func() { done <- supervisor.stopProcess(process) }()
	err := receiveStopResult(t, done)
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("stopProcess() error = %v, want Unavailable", err)
	}
	if got := process.killCount(); got != 1 {
		t.Fatalf("Kill() calls = %d, want 1", got)
	}
}

func TestSupervisorStopProcessIsBoundedWhenKilledProcessNeverJoins(t *testing.T) {
	closedTimer := make(chan time.Time)
	close(closedTimer)
	supervisor := &Supervisor{config: Config{StopTimeout: time.Second, after: func(time.Duration) <-chan time.Time { return closedTimer }}}
	process := &fakeProcess{done: make(chan error), hangOnSignal: true, hangOnKill: true}

	done := make(chan error, 1)
	go func() { done <- supervisor.stopProcess(process) }()
	err := receiveStopResult(t, done)
	if !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("stopProcess() error = %v, want DeadlineExceeded", err)
	}
	if got := process.killCount(); got != 1 {
		t.Fatalf("Kill() calls = %d, want 1", got)
	}
}

func receiveStopResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("stopProcess() exceeded its shutdown bound")
		return nil
	}
}

func newTestSupervisor(t *testing.T, permissions *privacy.Service, leases *fakeLeases, launcher *fakeLauncher, now time.Time) *Supervisor {
	t.Helper()
	supervisor, err := New(testConfig(), permissions, leases, launcher, engineclock.NewFake(now), testSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return supervisor
}

func testSpecs() []Spec {
	return []Spec{
		{ProviderID: "desktop-presence", Permission: privacy.CameraCapture, Command: "/python", Args: []string{"camera.py"}},
		{ProviderID: "desktop-vad", Permission: privacy.MicrophoneCapture, Command: "/python", Args: []string{"microphone.py"}},
	}
}

func testConfig() Config {
	instances := make(map[string]int)
	return Config{
		StopTimeout: time.Second, HealthCheckInterval: time.Second,
		newInstanceID: func(providerID string) (string, error) {
			instances[providerID]++
			return providerID + "-instance-" + strconv.Itoa(instances[providerID]), nil
		},
	}
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

type reorderingPermissions struct {
	mu              sync.Mutex
	snapshot        privacy.Snapshot
	enablePersisted chan struct{}
	releaseEnable   chan struct{}
	currentCalls    chan struct{}
}

func newReorderingPermissions() *reorderingPermissions {
	return &reorderingPermissions{
		snapshot: permissionSnapshot(0, false), enablePersisted: make(chan struct{}), releaseEnable: make(chan struct{}), currentCalls: make(chan struct{}, 4),
	}
}

func (p *reorderingPermissions) Current(context.Context) (privacy.Snapshot, error) {
	p.mu.Lock()
	snapshot := p.snapshot
	snapshot.Grants = append([]privacy.Grant(nil), snapshot.Grants...)
	p.mu.Unlock()
	p.currentCalls <- struct{}{}
	return snapshot, nil
}

func (p *reorderingPermissions) Change(_ context.Context, command privacy.ChangePermission) (privacy.Snapshot, error) {
	p.mu.Lock()
	if command.Enabled {
		p.snapshot = permissionSnapshot(1, true)
	} else {
		p.snapshot = permissionSnapshot(2, false)
	}
	snapshot := p.snapshot
	snapshot.Grants = append([]privacy.Grant(nil), snapshot.Grants...)
	p.mu.Unlock()
	if command.Enabled {
		close(p.enablePersisted)
		<-p.releaseEnable
	}
	return snapshot, nil
}

type cancelingPermissions struct {
	cancel   context.CancelFunc
	snapshot privacy.Snapshot
}

func (p *cancelingPermissions) Current(context.Context) (privacy.Snapshot, error) {
	return p.snapshot, nil
}
func (p *cancelingPermissions) Change(context.Context, privacy.ChangePermission) (privacy.Snapshot, error) {
	p.cancel()
	return p.snapshot, nil
}

func permissionSnapshot(revision uint64, camera bool) privacy.Snapshot {
	return privacy.Snapshot{Revision: revision, Grants: []privacy.Grant{{Permission: privacy.CameraCapture, Enabled: camera}}}
}

type fakeLeases struct {
	mu      sync.Mutex
	current []readiness.ProviderSnapshot
	updates chan []readiness.ProviderSnapshot
	revoked []string
}

func (f *fakeLeases) RevokeProvider(providerID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, providerID)
	return true
}
func (f *fakeLeases) revokedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
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
	mu           sync.Mutex
	done         chan error
	sent         bool
	signalsSeen  []os.Signal
	hangOnSignal bool
	hangOnKill   bool
	killErr      error
	killsSeen    int
}

func (f *fakeProcess) Signal(signal os.Signal) error {
	f.mu.Lock()
	f.signalsSeen = append(f.signalsSeen, signal)
	f.mu.Unlock()
	if !f.hangOnSignal {
		f.exit(nil)
	}
	return nil
}
func (f *fakeProcess) Kill() error {
	f.mu.Lock()
	f.killsSeen++
	f.mu.Unlock()
	if f.killErr != nil {
		return f.killErr
	}
	if !f.hangOnKill {
		f.exit(nil)
	}
	return nil
}
func (f *fakeProcess) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killsSeen
}
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
