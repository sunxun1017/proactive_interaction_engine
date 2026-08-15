package supervisor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
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
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 2)}
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
	if spec.ProcessID != "camera-process" || spec.BasePermission != privacy.CameraCapture {
		t.Fatalf("started spec = %#v", spec)
	}
	if spec.InstanceID != "camera-process-instance-1" {
		t.Fatalf("instance id = %q", spec.InstanceID)
	}
	if !reflect.DeepEqual(spec.Providers, []LogicalProviderSpec{{ProviderID: "desktop-presence", Capability: readiness.PersonPresence}}) {
		t.Fatalf("started providers = %#v", spec.Providers)
	}
	requireProviderState(t, supervisor, "desktop-presence", provider.Starting, provider.ReasonNone)

	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: spec.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy,
		HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute),
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

func TestSupervisorMapsProviderHealthReasonsExactly(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture, Enabled: true}); err != nil {
		t.Fatalf("seed camera permission: %v", err)
	}
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 2)}
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	spec := receiveStart(t, launcher.starts)

	tests := []struct {
		reason     readiness.ProviderHealthReason
		wantState  provider.State
		wantReason provider.Reason
	}{
		{readiness.ProviderHealthReasonStarting, provider.Starting, provider.ReasonNone},
		{readiness.ProviderHealthReasonDeviceUnavailable, provider.Degraded, provider.ReasonDeviceUnavailable},
		{readiness.ProviderHealthReasonPermissionDenied, provider.Degraded, provider.ReasonPermissionDenied},
		{readiness.ProviderHealthReasonDependencyUnavailable, provider.Degraded, provider.ReasonDependencyUnavailable},
		{readiness.ProviderHealthReasonModelUnavailable, provider.Degraded, provider.ReasonModelUnavailable},
		{readiness.ProviderHealthReasonInternalError, provider.Degraded, provider.ReasonInternalError},
		{readiness.ProviderHealthReasonShuttingDown, provider.Stopping, provider.ReasonShuttingDown},
	}
	for _, test := range tests {
		t.Run(string(test.reason), func(t *testing.T) {
			leases.publish([]readiness.ProviderSnapshot{{
				ProviderID: "desktop-presence", InstanceID: spec.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
				Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Unhealthy,
				HealthReason: test.reason, LeaseExpiresAt: now.Add(time.Minute),
			}})
			requireProviderState(t, supervisor, "desktop-presence", test.wantState, test.wantReason)
		})
	}
}

func TestSupervisorCrashDegradesWithoutRestartUntilExplicitRetry(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.MicrophoneCapture, Enabled: true}); err != nil {
		t.Fatalf("seed microphone permission: %v", err)
	}
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 3)}
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
	if _, err := supervisor.Change(context.Background(), privacy.ChangePermission{Permission: privacy.SpeechTranscription, Enabled: true}); err != nil {
		t.Fatalf("change unrelated permission: %v", err)
	}
	select {
	case spec := <-launcher.starts:
		t.Fatalf("unrelated permission retried crashed worker: %#v", spec)
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

func TestSupervisorProcessCrashOnlyDegradesEnabledProvidersInItsGroup(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.MicrophoneCapture} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("seed %s: %v", permission, err)
		}
	}
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 3)}
	source := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	camera := receiveStart(t, launcher.starts)
	microphone := receiveStart(t, launcher.starts)
	if camera.ProcessID != "camera-process" || microphone.ProcessID != "microphone-process" {
		t.Fatalf("process start order = %q, %q", camera.ProcessID, microphone.ProcessID)
	}
	leases.publish([]readiness.ProviderSnapshot{
		{ProviderID: "desktop-presence", InstanceID: camera.InstanceID, Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute)},
		{ProviderID: "desktop-vad", InstanceID: microphone.InstanceID, Capabilities: []readiness.CapabilityKind{readiness.VoiceActivity}, Health: readiness.Healthy, HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute)},
	})
	requireProviderState(t, source, "desktop-presence", provider.Running, provider.ReasonNone)
	requireProviderState(t, source, "desktop-vad", provider.Running, provider.ReasonNone)

	launcher.process(0).exit(errors.New("native camera crash"))
	requireProviderState(t, source, "desktop-presence", provider.Degraded, provider.ReasonInternalError)
	requireProviderState(t, source, "desktop-vad", provider.Running, provider.ReasonNone)
	if got := leases.revokedSnapshot(); !reflect.DeepEqual(got, []string{"desktop-presence"}) {
		t.Fatalf("revoked providers = %#v, want camera group only", got)
	}
}

func TestSupervisorShutdownStopsAndJoinsEveryStartedWorker(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.MicrophoneCapture} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("seed %s permission: %v", permission, err)
		}
	}
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 3)}
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
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 2)}
	config := testConfig()
	supervisor, err := New(config, permissions, leases, launcher, clock, []ProcessSpec{
		{ProcessID: "camera-process", BasePermission: privacy.CameraCapture, Command: "/python", Providers: []LogicalProviderSpec{{ProviderID: "desktop-presence", Capability: readiness.PersonPresence}}},
		{ProcessID: "microphone-process", BasePermission: privacy.MicrophoneCapture, Command: "/python", Providers: []LogicalProviderSpec{{ProviderID: "desktop-vad", Capability: readiness.VoiceActivity}}},
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
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy,
		HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Second),
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
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy,
		HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute),
	}})
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 2)}
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	spec := receiveStart(t, launcher.starts)
	requireProviderState(t, supervisor, "desktop-presence", provider.Starting, provider.ReasonNone)
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: spec.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy,
		HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute),
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
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 3)}
	supervisor := newTestSupervisor(t, permissions, leases, launcher, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := receiveStart(t, launcher.starts)
	leases.publish([]readiness.ProviderSnapshot{{
		ProviderID: "desktop-presence", InstanceID: first.InstanceID, ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy,
		HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute),
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
	supervisor, err := New(testConfig(), permissions, newFakeLeases(), &fakeLauncher{starts: make(chan ProcessSpec, 2)}, engineclock.NewFake(now), testSpecs())
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

func TestNewRequiresExactlyTwoStrictDeviceProcessGroups(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	newSupervisor := func(specs []ProcessSpec) error {
		_, err := New(testConfig(), newPermissionService(t, now), newFakeLeases(), &fakeLauncher{starts: make(chan ProcessSpec, 1)}, engineclock.NewFake(now), specs)
		return err
	}
	if err := newSupervisor(fullTestSpecs()); err != nil {
		t.Fatalf("New(valid process groups) error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func([]ProcessSpec) []ProcessSpec
	}{
		{name: "one group", mutate: func(specs []ProcessSpec) []ProcessSpec { return specs[:1] }},
		{name: "duplicate process id", mutate: func(specs []ProcessSpec) []ProcessSpec { specs[1].ProcessID = specs[0].ProcessID; return specs }},
		{name: "process provider id collision", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[0].ProcessID = specs[1].Providers[0].ProviderID
			return specs
		}},
		{name: "duplicate provider id", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[1].Providers[0].ProviderID = specs[0].Providers[0].ProviderID
			return specs
		}},
		{name: "duplicate capability", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[0].Providers[1].Capability = readiness.PersonPresence
			return specs
		}},
		{name: "missing camera base", mutate: func(specs []ProcessSpec) []ProcessSpec { specs[0].Providers = specs[0].Providers[1:]; return specs }},
		{name: "missing microphone base", mutate: func(specs []ProcessSpec) []ProcessSpec { specs[1].Providers = specs[1].Providers[1:]; return specs }},
		{name: "audio capability in camera group", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[0].Providers[1].Capability = readiness.SpeakerIdentification
			return specs
		}},
		{name: "vision capability in microphone group", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[1].Providers[1].Capability = readiness.FaceDetection
			return specs
		}},
		{name: "unknown base permission", mutate: func(specs []ProcessSpec) []ProcessSpec { specs[0].BasePermission = privacy.FaceDetection; return specs }},
		{name: "supervisor instance id supplied", mutate: func(specs []ProcessSpec) []ProcessSpec { specs[0].InstanceID = "caller-owned"; return specs }},
		{name: "blank logical provider id", mutate: func(specs []ProcessSpec) []ProcessSpec { specs[0].Providers[0].ProviderID = ""; return specs }},
		{name: "oversized process id", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[0].ProcessID = strings.Repeat("p", maxSupervisorIDBytes+1)
			return specs
		}},
		{name: "oversized provider id", mutate: func(specs []ProcessSpec) []ProcessSpec {
			specs[0].Providers[0].ProviderID = strings.Repeat("p", maxSupervisorIDBytes+1)
			return specs
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := newSupervisor(test.mutate(cloneTestProcessSpecs(fullTestSpecs()))); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("New() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestSupervisorDependentOnlyDoesNotStartAndAllLogicalProvidersAreSorted(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: privacy.FaceIdentification, Enabled: true}); err != nil {
		t.Fatalf("enable dependent permission: %v", err)
	}
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 1)}
	source, err := New(testConfig(), permissions, newFakeLeases(), launcher, engineclock.NewFake(now), fullTestSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	wantIDs := []string{"desktop-face-detection", "desktop-face-identification", "desktop-face-liveness", "desktop-presence", "desktop-speaker-identification", "desktop-speaker-verification", "desktop-vad"}
	snapshot := source.CurrentProviderRuntime()
	gotIDs := make([]string, 0, len(snapshot.Providers))
	for _, item := range snapshot.Providers {
		gotIDs = append(gotIDs, item.ProviderID)
		wantReason := provider.ReasonPermissionDenied
		if item.ProviderID == "desktop-presence" || item.ProviderID == "desktop-vad" {
			wantReason = provider.ReasonDisabledByUser
		}
		if item.State != provider.Disabled || item.Reason != wantReason {
			t.Fatalf("provider %q runtime = %#v, want Disabled/%s", item.ProviderID, item, wantReason)
		}
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("provider order = %#v, want %#v", gotIDs, wantIDs)
	}
	select {
	case spec := <-launcher.starts:
		t.Fatalf("dependent-only permission started process: %#v", spec)
	default:
	}
}

func TestSupervisorRestartsWholeGroupWhenEffectiveLogicalProvidersChange(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("seed %s: %v", permission, err)
		}
	}
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 3)}
	source, err := New(testConfig(), permissions, leases, launcher, engineclock.NewFake(now), fullTestSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := receiveStart(t, launcher.starts)
	wantFirstProviders := []LogicalProviderSpec{
		{ProviderID: "desktop-face-detection", Capability: readiness.FaceDetection},
		{ProviderID: "desktop-face-identification", Capability: readiness.FaceIdentification},
		{ProviderID: "desktop-presence", Capability: readiness.PersonPresence},
	}
	if first.ProcessID != "camera-process" || !reflect.DeepEqual(first.Providers, wantFirstProviders) {
		t.Fatalf("first start = %#v, want authorized camera providers", first)
	}
	leases.publish([]readiness.ProviderSnapshot{
		{ProviderID: "desktop-face-detection", InstanceID: first.InstanceID, Capabilities: []readiness.CapabilityKind{readiness.FaceDetection}, Health: readiness.Unhealthy, HealthReason: readiness.ProviderHealthReasonModelUnavailable, LeaseExpiresAt: now.Add(time.Minute)},
		{ProviderID: "desktop-face-identification", InstanceID: first.InstanceID, Capabilities: []readiness.CapabilityKind{readiness.FaceIdentification}, Health: readiness.Healthy, HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute)},
		{ProviderID: "desktop-presence", InstanceID: first.InstanceID, Capabilities: []readiness.CapabilityKind{readiness.PersonPresence}, Health: readiness.Healthy, HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute)},
	})
	requireProviderState(t, source, "desktop-face-detection", provider.Degraded, provider.ReasonModelUnavailable)
	requireProviderState(t, source, "desktop-face-identification", provider.Running, provider.ReasonNone)
	requireProviderState(t, source, "desktop-presence", provider.Running, provider.ReasonNone)

	if _, err := source.Change(context.Background(), privacy.ChangePermission{Permission: privacy.FaceLiveness, Enabled: true}); err != nil {
		t.Fatalf("enable face liveness: %v", err)
	}
	second := receiveStart(t, launcher.starts)
	if second.InstanceID == first.InstanceID || len(second.Providers) != 4 {
		t.Fatalf("second start = %#v, want fresh complete camera group", second)
	}
	if got := launcher.process(0).signals(); !reflect.DeepEqual(got, []os.Signal{syscall.SIGTERM}) {
		t.Fatalf("old process signals = %#v, want SIGTERM", got)
	}
	wantRevoked := []string{"desktop-face-detection", "desktop-face-identification", "desktop-face-liveness", "desktop-presence"}
	if got := leases.revokedSnapshot(); !reflect.DeepEqual(got, wantRevoked) {
		t.Fatalf("revoked providers = %#v, want whole group %#v", got, wantRevoked)
	}
	for _, logical := range second.Providers {
		requireProviderState(t, source, logical.ProviderID, provider.Starting, provider.ReasonNone)
	}
}

func TestSupervisorBaseDisableRevokesWholeGroupAndDoesNotStartDependents(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("seed %s: %v", permission, err)
		}
	}
	leases := newFakeLeases()
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 2)}
	source, err := New(testConfig(), permissions, leases, launcher, engineclock.NewFake(now), fullTestSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	receiveStart(t, launcher.starts)

	if _, err := source.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture}); err != nil {
		t.Fatalf("disable camera: %v", err)
	}
	requireProviderState(t, source, "desktop-presence", provider.Disabled, provider.ReasonDisabledByUser)
	for _, id := range []string{"desktop-face-detection", "desktop-face-identification", "desktop-face-liveness"} {
		requireProviderState(t, source, id, provider.Disabled, provider.ReasonPermissionDenied)
	}
	wantRevoked := []string{"desktop-face-detection", "desktop-face-identification", "desktop-face-liveness", "desktop-presence"}
	if got := leases.revokedSnapshot(); !reflect.DeepEqual(got, wantRevoked) {
		t.Fatalf("revoked providers = %#v, want %#v", got, wantRevoked)
	}
	select {
	case spec := <-launcher.starts:
		t.Fatalf("base-disabled dependents started process: %#v", spec)
	default:
	}
}

func TestSupervisorRereadsPersistedPermissionAfterBlockedGroupStop(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	permissions := newPermissionService(t, now)
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("seed %s: %v", permission, err)
		}
	}
	stopTimer := make(chan time.Time, 1)
	config := testConfig()
	config.after = func(time.Duration) <-chan time.Time { return stopTimer }
	launcher := &fakeLauncher{starts: make(chan ProcessSpec, 3)}
	source, err := New(config, permissions, newFakeLeases(), launcher, engineclock.NewFake(now), fullTestSpecs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	receiveStart(t, launcher.starts)
	process := launcher.process(0)
	process.mu.Lock()
	process.hangOnSignal = true
	process.mu.Unlock()

	if _, err := source.Change(context.Background(), privacy.ChangePermission{Permission: privacy.FaceLiveness, Enabled: true}); err != nil {
		t.Fatalf("enable liveness: %v", err)
	}
	waitForSignalCount(t, process, 1)
	if _, err := source.Change(context.Background(), privacy.ChangePermission{Permission: privacy.CameraCapture}); err != nil {
		t.Fatalf("disable camera while old process is stopping: %v", err)
	}
	stopTimer <- now
	requireProviderState(t, source, "desktop-presence", provider.Disabled, provider.ReasonDisabledByUser)
	for _, id := range []string{"desktop-face-detection", "desktop-face-identification", "desktop-face-liveness"} {
		requireProviderState(t, source, id, provider.Disabled, provider.ReasonPermissionDenied)
	}
	select {
	case spec := <-launcher.starts:
		t.Fatalf("stale authorized providers started after stricter permission persisted: %#v", spec)
	default:
	}
}

func TestSupervisorChangeReturnsPersistedSnapshotAfterContextCancellation(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	permissions := &cancelingPermissions{cancel: cancel, snapshot: permissionSnapshot(1, true)}
	supervisor, err := New(testConfig(), permissions, newFakeLeases(), &fakeLauncher{starts: make(chan ProcessSpec, 2)}, engineclock.NewFake(now), testSpecs())
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

func testSpecs() []ProcessSpec {
	return []ProcessSpec{
		{ProcessID: "camera-process", BasePermission: privacy.CameraCapture, Command: "/python", Args: []string{"camera.py"}, Providers: []LogicalProviderSpec{{ProviderID: "desktop-presence", Capability: readiness.PersonPresence}}},
		{ProcessID: "microphone-process", BasePermission: privacy.MicrophoneCapture, Command: "/python", Args: []string{"microphone.py"}, Providers: []LogicalProviderSpec{{ProviderID: "desktop-vad", Capability: readiness.VoiceActivity}}},
	}
}

func fullTestSpecs() []ProcessSpec {
	return []ProcessSpec{
		{
			ProcessID: "camera-process", BasePermission: privacy.CameraCapture, Command: "/python", Args: []string{"camera.py"},
			Providers: []LogicalProviderSpec{
				{ProviderID: "desktop-presence", Capability: readiness.PersonPresence},
				{ProviderID: "desktop-face-detection", Capability: readiness.FaceDetection},
				{ProviderID: "desktop-face-identification", Capability: readiness.FaceIdentification},
				{ProviderID: "desktop-face-liveness", Capability: readiness.FaceLiveness},
			},
		},
		{
			ProcessID: "microphone-process", BasePermission: privacy.MicrophoneCapture, Command: "/python", Args: []string{"microphone.py"},
			Providers: []LogicalProviderSpec{
				{ProviderID: "desktop-vad", Capability: readiness.VoiceActivity},
				{ProviderID: "desktop-speaker-identification", Capability: readiness.SpeakerIdentification},
				{ProviderID: "desktop-speaker-verification", Capability: readiness.SpeakerVerification},
			},
		},
	}
}

func cloneTestProcessSpecs(input []ProcessSpec) []ProcessSpec {
	output := append([]ProcessSpec(nil), input...)
	for index := range output {
		output[index].Args = append([]string(nil), output[index].Args...)
		output[index].Env = append([]string(nil), output[index].Env...)
		output[index].Providers = append([]LogicalProviderSpec(nil), output[index].Providers...)
	}
	return output
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

func waitForSignalCount(t *testing.T, process *fakeProcess, count int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if len(process.signals()) >= count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("process signals = %#v, want at least %d", process.signals(), count)
		default:
		}
	}
}

func receiveStart(t *testing.T, starts <-chan ProcessSpec) ProcessSpec {
	t.Helper()
	select {
	case spec := <-starts:
		return spec
	case <-time.After(time.Second):
		t.Fatal("worker was not started")
		return ProcessSpec{}
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
	starts    chan ProcessSpec
	processes []*fakeProcess
}

func (f *fakeLauncher) Start(spec ProcessSpec) (Process, error) {
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
