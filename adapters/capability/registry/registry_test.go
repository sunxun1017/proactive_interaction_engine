package registry

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ platformv1.CapabilityProviderRegistryServiceServer = (*Registry)(nil)

func TestNewValidatesConstruction(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	for _, test := range []struct {
		name          string
		clock         port.Clock
		leaseDuration time.Duration
	}{
		{name: "nil clock", leaseDuration: time.Minute},
		{name: "zero lease", clock: clock},
		{name: "negative lease", clock: clock, leaseDuration: -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if registry, err := New(test.clock, test.leaseDuration); err == nil || registry != nil {
				t.Fatalf("New() = %#v, %v, want nil and error", registry, err)
			}
		})
	}
}

func TestRegisterMapsEveryServiceCapabilityExplicitly(t *testing.T) {
	mappings := []struct {
		wire platformv1.ServiceCapabilityKind
		app  readiness.CapabilityKind
	}{
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE, readiness.PersonPresence},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION, readiness.FaceDetection},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION, readiness.FaceIdentification},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_LIVENESS, readiness.FaceLiveness},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY, readiness.VoiceActivity},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION, readiness.SpeakerIdentification},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION, readiness.SpeakerVerification},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEECH_TRANSCRIPTION, readiness.SpeechTranscription},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_ATTENTION_ESTIMATION, readiness.AttentionEstimation},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_BUSY_STATE, readiness.BusyState},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_GESTURE_DETECTION, readiness.GestureDetection},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_DEVICE_STATE, readiness.DeviceState},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_DISPLAY_TEXT, readiness.DisplayText},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_AVATAR_ATTEND, readiness.AvatarAttend},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_AVATAR_EXPRESSION, readiness.AvatarExpression},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEECH_SYNTHESIS, readiness.SpeechSynthesis},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_GESTURE, readiness.Gesture},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_LIGHT, readiness.Light},
		{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_LOCOMOTION, readiness.Locomotion},
	}

	registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
	request := validRegistration("all", "instance", mappings[0].wire)
	request.Capabilities = make([]platformv1.ServiceCapabilityKind, 0, len(mappings))
	want := make([]readiness.CapabilityKind, 0, len(mappings))
	for _, mapping := range mappings {
		request.Capabilities = append(request.Capabilities, mapping.wire)
		want = append(want, mapping.app)
	}
	registerOK(t, registry, request)

	snapshots := registry.Snapshots()
	if len(snapshots) != 1 || !reflect.DeepEqual(snapshots[0].Capabilities, want) {
		t.Fatalf("Snapshots() capabilities = %#v, want %#v", snapshots, want)
	}
}

func TestRegisterRejectsInvalidRequestsWithStableCodes(t *testing.T) {
	now := testNow()
	tests := []struct {
		name     string
		request  *platformv1.RegisterCapabilityProviderRequest
		wantCode codes.Code
	}{
		{name: "nil request", wantCode: codes.InvalidArgument},
		{name: "empty provider id", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) { r.ProviderId = "" }), wantCode: codes.InvalidArgument},
		{name: "empty instance id", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) { r.InstanceId = "" }), wantCode: codes.InvalidArgument},
		{name: "empty protocol", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) { r.ProtocolVersion = "" }), wantCode: codes.InvalidArgument},
		{name: "unknown protocol", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) { r.ProtocolVersion = "v2" }), wantCode: codes.FailedPrecondition},
		{name: "empty implementation version", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) { r.ImplementationVersion = "" }), wantCode: codes.InvalidArgument},
		{name: "empty capabilities", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) { r.Capabilities = nil }), wantCode: codes.InvalidArgument},
		{name: "duplicate capability", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Capabilities = append(r.Capabilities, r.Capabilities[0])
		}), wantCode: codes.InvalidArgument},
		{name: "unspecified capability", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Capabilities = []platformv1.ServiceCapabilityKind{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_UNSPECIFIED}
		}), wantCode: codes.InvalidArgument},
		{name: "unknown capability", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Capabilities = []platformv1.ServiceCapabilityKind{platformv1.ServiceCapabilityKind(99)}
		}), wantCode: codes.InvalidArgument},
		{name: "unspecified health", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNSPECIFIED
		}), wantCode: codes.InvalidArgument},
		{name: "unknown health", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState(99)
		}), wantCode: codes.InvalidArgument},
		{name: "unspecified reason", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.HealthReason = platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_UNSPECIFIED
		}), wantCode: codes.InvalidArgument},
		{name: "unknown reason", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.HealthReason = platformv1.ProviderHealthReason(99)
		}), wantCode: codes.InvalidArgument},
		{name: "healthy with failure reason", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.HealthReason = platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE
		}), wantCode: codes.InvalidArgument},
		{name: "unhealthy with none reason", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNHEALTHY
		}), wantCode: codes.InvalidArgument},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry, err := New(engineclock.NewFake(now), time.Minute)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			_, err = registry.RegisterCapabilityProvider(context.Background(), test.request)
			requireStatusCode(t, err, test.wantCode)
			if snapshots := registry.Snapshots(); len(snapshots) != 0 {
				t.Fatalf("invalid register mutated snapshots: %#v", snapshots)
			}
		})
	}
}

func TestRegisterUsesServerTimeAndIsIdempotent(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, 45*time.Second)
	request := validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
	request.Capabilities = append(request.Capabilities, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION)

	first := registerOK(t, registry, request)
	if first.LeaseId == "" || first.ExpiresAt.AsTime() != testNow().Add(45*time.Second) {
		t.Fatalf("first register = %#v, want server-issued lease expiring at %s", first, testNow().Add(45*time.Second))
	}
	clock.Advance(10 * time.Second)
	reordered := validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION)
	reordered.Capabilities = append(reordered.Capabilities, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
	second := registerOK(t, registry, reordered)
	if second.LeaseId != first.LeaseId || !second.ExpiresAt.AsTime().Equal(first.ExpiresAt.AsTime()) {
		t.Fatalf("idempotent register = %#v, want original lease %#v", second, first)
	}
}

func TestRegisterRejectsActiveProviderConflicts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*platformv1.RegisterCapabilityProviderRequest)
	}{
		{name: "different instance", mutate: func(r *platformv1.RegisterCapabilityProviderRequest) { r.InstanceId = "other" }},
		{name: "changed implementation", mutate: func(r *platformv1.RegisterCapabilityProviderRequest) { r.ImplementationVersion = "v2" }},
		{name: "changed capability", mutate: func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Capabilities = []platformv1.ServiceCapabilityKind{platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION}
		}},
		{name: "changed health", mutate: func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNHEALTHY
			r.HealthReason = platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
			original := validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
			registerOK(t, registry, original)
			changed := validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
			test.mutate(changed)
			_, err := registry.RegisterCapabilityProvider(context.Background(), changed)
			requireStatusCode(t, err, codes.AlreadyExists)
		})
	}
}

func TestLeaseSnapshotReturnsIndependentActiveAndExpiredRecords(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	registration := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))

	for _, leaseID := range []string{"", "unknown"} {
		if snapshot, ok := registry.LeaseSnapshot(leaseID); ok {
			t.Fatalf("LeaseSnapshot(%q) = %#v, true, want false", leaseID, snapshot)
		}
	}
	first, ok := registry.LeaseSnapshot(registration.LeaseId)
	if !ok || first.ProviderID != "camera" || first.InstanceID != "camera-1" || !reflect.DeepEqual(first.Capabilities, []readiness.CapabilityKind{readiness.PersonPresence}) {
		t.Fatalf("LeaseSnapshot(active) = %#v, %t", first, ok)
	}
	first.Capabilities[0] = readiness.Locomotion
	second, ok := registry.LeaseSnapshot(registration.LeaseId)
	if !ok || !reflect.DeepEqual(second.Capabilities, []readiness.CapabilityKind{readiness.PersonPresence}) {
		t.Fatalf("LeaseSnapshot(after returned mutation) = %#v, %t", second, ok)
	}

	clock.Advance(time.Minute)
	expired, ok := registry.LeaseSnapshot(registration.LeaseId)
	if !ok || !reflect.DeepEqual(expired, second) {
		t.Fatalf("LeaseSnapshot(expired) = %#v, %t, want %#v, true", expired, ok, second)
	}
}

func TestExpiredProviderCanBeReplacedButOldLeaseCannotRevive(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	old := registerOK(t, registry, validRegistration("camera", "old", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
	clock.Advance(time.Minute)

	replacement := registerOK(t, registry, validRegistration("camera", "new", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION))
	if replacement.LeaseId == old.LeaseId {
		t.Fatalf("replacement lease id = old lease id %q", old.LeaseId)
	}
	if snapshot, ok := registry.LeaseSnapshot(old.LeaseId); ok {
		t.Fatalf("LeaseSnapshot(old) = %#v, true after replacement", snapshot)
	}
	if snapshot, ok := registry.LeaseSnapshot(replacement.LeaseId); !ok || snapshot.InstanceID != "new" {
		t.Fatalf("LeaseSnapshot(replacement) = %#v, %t", snapshot, ok)
	}
	_, err := registry.HeartbeatCapabilityProvider(context.Background(), healthyHeartbeat(old.LeaseId))
	requireStatusCode(t, err, codes.NotFound)
	snapshots := registry.Snapshots()
	if len(snapshots) != 1 || snapshots[0].InstanceID != "new" || snapshots[0].Capabilities[0] != readiness.FaceDetection {
		t.Fatalf("Snapshots() after replacement = %#v", snapshots)
	}
}

func TestHeartbeatRenewsFromServerTimeAndUpdatesHealth(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	registration := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
	clock.Advance(20 * time.Second)

	response, err := registry.HeartbeatCapabilityProvider(context.Background(), &platformv1.HeartbeatCapabilityProviderRequest{
		LeaseId:      registration.LeaseId,
		Health:       platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNHEALTHY,
		HealthReason: platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE,
	})
	if err != nil {
		t.Fatalf("HeartbeatCapabilityProvider() error = %v", err)
	}
	wantExpiry := testNow().Add(80 * time.Second)
	if response.ExpiresAt.AsTime() != wantExpiry {
		t.Fatalf("heartbeat expiry = %s, want %s", response.ExpiresAt.AsTime(), wantExpiry)
	}
	snapshots := registry.Snapshots()
	if len(snapshots) != 1 || snapshots[0].Health != readiness.Unhealthy || snapshots[0].LeaseExpiresAt != wantExpiry {
		t.Fatalf("Snapshots() after heartbeat = %#v", snapshots)
	}
}

func TestHeartbeatRejectsInvalidLeaseAndHealthWithoutMutation(t *testing.T) {
	tests := []struct {
		name     string
		request  func(string) *platformv1.HeartbeatCapabilityProviderRequest
		wantCode codes.Code
	}{
		{name: "nil request", wantCode: codes.InvalidArgument},
		{name: "empty lease", request: func(string) *platformv1.HeartbeatCapabilityProviderRequest { return healthyHeartbeat("") }, wantCode: codes.InvalidArgument},
		{name: "unknown lease", request: func(string) *platformv1.HeartbeatCapabilityProviderRequest { return healthyHeartbeat("unknown") }, wantCode: codes.NotFound},
		{name: "unspecified health", request: heartbeatWith(func(r *platformv1.HeartbeatCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNSPECIFIED
		}), wantCode: codes.InvalidArgument},
		{name: "unknown health", request: heartbeatWith(func(r *platformv1.HeartbeatCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState(99)
		}), wantCode: codes.InvalidArgument},
		{name: "unspecified reason", request: heartbeatWith(func(r *platformv1.HeartbeatCapabilityProviderRequest) {
			r.HealthReason = platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_UNSPECIFIED
		}), wantCode: codes.InvalidArgument},
		{name: "unknown reason", request: heartbeatWith(func(r *platformv1.HeartbeatCapabilityProviderRequest) {
			r.HealthReason = platformv1.ProviderHealthReason(99)
		}), wantCode: codes.InvalidArgument},
		{name: "healthy with failure reason", request: heartbeatWith(func(r *platformv1.HeartbeatCapabilityProviderRequest) {
			r.HealthReason = platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_MODEL_UNAVAILABLE
		}), wantCode: codes.InvalidArgument},
		{name: "unhealthy with none reason", request: heartbeatWith(func(r *platformv1.HeartbeatCapabilityProviderRequest) {
			r.Health = platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNHEALTHY
		}), wantCode: codes.InvalidArgument},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
			registration := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
			before := registry.Snapshots()[0]
			var request *platformv1.HeartbeatCapabilityProviderRequest
			if test.request != nil {
				request = test.request(registration.LeaseId)
			}
			_, err := registry.HeartbeatCapabilityProvider(context.Background(), request)
			requireStatusCode(t, err, test.wantCode)
			if after := registry.Snapshots()[0]; !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid heartbeat changed snapshot: before %#v, after %#v", before, after)
			}
		})
	}
}

func TestHeartbeatAtExactExpiryCannotRenewAndExpiredSnapshotRemains(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	registration := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
	clock.Advance(time.Minute)
	before := registry.Snapshots()[0]

	_, err := registry.HeartbeatCapabilityProvider(context.Background(), healthyHeartbeat(registration.LeaseId))
	requireStatusCode(t, err, codes.DeadlineExceeded)
	after := registry.Snapshots()
	if len(after) != 1 || !reflect.DeepEqual(after[0], before) {
		t.Fatalf("expired heartbeat changed snapshot: before %#v, after %#v", before, after)
	}
}

func TestUnregisterValidatesLeaseAndRemovesProvider(t *testing.T) {
	invalid := []struct {
		name     string
		request  *platformv1.UnregisterCapabilityProviderRequest
		wantCode codes.Code
	}{
		{name: "nil request", wantCode: codes.InvalidArgument},
		{name: "empty lease", request: &platformv1.UnregisterCapabilityProviderRequest{}, wantCode: codes.InvalidArgument},
		{name: "unknown lease", request: &platformv1.UnregisterCapabilityProviderRequest{LeaseId: "unknown"}, wantCode: codes.NotFound},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
			registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
			before := registry.Snapshots()[0]
			_, err := registry.UnregisterCapabilityProvider(context.Background(), test.request)
			requireStatusCode(t, err, test.wantCode)
			if after := registry.Snapshots(); len(after) != 1 || !reflect.DeepEqual(after[0], before) {
				t.Fatalf("invalid unregister changed snapshots: before %#v, after %#v", before, after)
			}
		})
	}

	t.Run("registered lease", func(t *testing.T) {
		registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
		registration := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
		if _, err := registry.UnregisterCapabilityProvider(context.Background(), &platformv1.UnregisterCapabilityProviderRequest{LeaseId: registration.LeaseId}); err != nil {
			t.Fatalf("UnregisterCapabilityProvider() error = %v", err)
		}
		if snapshots := registry.Snapshots(); len(snapshots) != 0 {
			t.Fatalf("Snapshots() after unregister = %#v, want empty", snapshots)
		}
		if snapshot, ok := registry.LeaseSnapshot(registration.LeaseId); ok {
			t.Fatalf("LeaseSnapshot(unregistered) = %#v, true", snapshot)
		}
	})
}

func TestSnapshotsAreSortedAndDeepCopied(t *testing.T) {
	registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
	registerOK(t, registry, validRegistration("z-provider", "z", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY))
	registerOK(t, registry, validRegistration("a-provider", "a", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))

	first := registry.Snapshots()
	if len(first) != 2 || first[0].ProviderID != "a-provider" || first[1].ProviderID != "z-provider" {
		t.Fatalf("Snapshots() order = %#v", first)
	}
	first[0].ProviderID = "mutated"
	first[0].Capabilities[0] = readiness.Locomotion
	second := registry.Snapshots()
	if second[0].ProviderID != "a-provider" || second[0].Capabilities[0] != readiness.PersonPresence {
		t.Fatalf("mutating returned snapshot changed registry: %#v", second)
	}
}

func TestRPCsHonorCancelledContextBeforeMutation(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	request := validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
	_, err := registry.RegisterCapabilityProvider(cancelled, request)
	requireStatusCode(t, err, codes.Canceled)
	if len(registry.Snapshots()) != 0 {
		t.Fatal("cancelled register mutated registry")
	}

	registration := registerOK(t, registry, request)
	before := registry.Snapshots()[0]
	_, err = registry.HeartbeatCapabilityProvider(cancelled, &platformv1.HeartbeatCapabilityProviderRequest{
		LeaseId:      registration.LeaseId,
		Health:       platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNHEALTHY,
		HealthReason: platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE,
	})
	requireStatusCode(t, err, codes.Canceled)
	if after := registry.Snapshots()[0]; !reflect.DeepEqual(after, before) {
		t.Fatalf("cancelled heartbeat changed snapshot: before %#v, after %#v", before, after)
	}

	_, err = registry.UnregisterCapabilityProvider(cancelled, &platformv1.UnregisterCapabilityProviderRequest{LeaseId: registration.LeaseId})
	requireStatusCode(t, err, codes.Canceled)
	if len(registry.Snapshots()) != 1 {
		t.Fatal("cancelled unregister removed provider")
	}
}

func TestRegistrySupportsConcurrentHeartbeatSnapshotAndIdempotentRegister(t *testing.T) {
	registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
	const providerCount = 12
	requests := make([]*platformv1.RegisterCapabilityProviderRequest, providerCount)
	leases := make([]string, providerCount)
	for i := range providerCount {
		requests[i] = validRegistration(providerID(i), "instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
		leases[i] = registerOK(t, registry, requests[i]).LeaseId
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, providerCount*2)
	var workers sync.WaitGroup
	for i := range providerCount {
		workers.Add(2)
		go func(index int) {
			defer workers.Done()
			<-start
			_, err := registry.HeartbeatCapabilityProvider(context.Background(), healthyHeartbeat(leases[index]))
			errorsSeen <- err
		}(i)
		go func(index int) {
			defer workers.Done()
			<-start
			_, err := registry.RegisterCapabilityProvider(context.Background(), requests[index])
			errorsSeen <- err
		}(i)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for range providerCount {
			_ = registry.Snapshots()
			_, _ = registry.LeaseSnapshot(leases[0])
		}
	}()
	close(start)
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent registry operation error = %v", err)
		}
	}
	if got := len(registry.Snapshots()); got != providerCount {
		t.Fatalf("Snapshots() count = %d, want %d", got, providerCount)
	}
}

func validRegistration(providerID, instanceID string, capability platformv1.ServiceCapabilityKind) *platformv1.RegisterCapabilityProviderRequest {
	return &platformv1.RegisterCapabilityProviderRequest{
		ProviderId:            providerID,
		InstanceId:            instanceID,
		ProtocolVersion:       SupportedProtocolVersion,
		ImplementationVersion: "test-v1",
		Capabilities:          []platformv1.ServiceCapabilityKind{capability},
		Health:                platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_HEALTHY,
		HealthReason:          platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE,
	}
}

func registrationWith(mutate func(*platformv1.RegisterCapabilityProviderRequest)) *platformv1.RegisterCapabilityProviderRequest {
	request := validRegistration("provider", "instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
	mutate(request)
	return request
}

func healthyHeartbeat(leaseID string) *platformv1.HeartbeatCapabilityProviderRequest {
	return &platformv1.HeartbeatCapabilityProviderRequest{
		LeaseId:      leaseID,
		Health:       platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_HEALTHY,
		HealthReason: platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE,
	}
}

func heartbeatWith(mutate func(*platformv1.HeartbeatCapabilityProviderRequest)) func(string) *platformv1.HeartbeatCapabilityProviderRequest {
	return func(leaseID string) *platformv1.HeartbeatCapabilityProviderRequest {
		request := healthyHeartbeat(leaseID)
		mutate(request)
		return request
	}
}

func registerOK(t *testing.T, registry *Registry, request *platformv1.RegisterCapabilityProviderRequest) *platformv1.RegisterCapabilityProviderResponse {
	t.Helper()
	response, err := registry.RegisterCapabilityProvider(context.Background(), request)
	if err != nil {
		t.Fatalf("RegisterCapabilityProvider() error = %v", err)
	}
	if response == nil || response.ExpiresAt == nil {
		t.Fatalf("RegisterCapabilityProvider() response = %#v, want lease and expiry", response)
	}
	return response
}

func requireStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("status.Code(%v) = %s, want %s", err, got, want)
	}
}

func providerID(index int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	return "provider-" + string(digits[index])
}

func testNow() time.Time {
	return time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
}
