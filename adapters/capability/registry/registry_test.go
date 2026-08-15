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
	"google.golang.org/protobuf/types/known/durationpb"
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

func TestRegisterMapsEveryOperationalProfileEnumExplicitly(t *testing.T) {
	privacyMappings := []struct {
		wire platformv1.ProviderPrivacyClass
		app  readiness.ProviderPrivacyClass
	}{
		{platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL, readiness.ProviderPrivacyDeviceLocal},
		{platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_REMOTE_PROCESSING, readiness.ProviderPrivacyRemoteProcessing},
	}
	for _, mapping := range privacyMappings {
		profile := validOperationalProfile()
		profile.PrivacyClass = mapping.wire
		got, err := normalizeOperationalProfile(profile)
		if err != nil || got.PrivacyClass != mapping.app {
			t.Fatalf("normalizeOperationalProfile(%s) privacy = %q, %v, want %q", mapping.wire, got.PrivacyClass, err, mapping.app)
		}
	}

	cancellationMappings := []struct {
		wire platformv1.ProviderCancellationSemantics
		app  readiness.ProviderCancellationSemantics
	}{
		{platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_NOT_SUPPORTED, readiness.ProviderCancellationNotSupported},
		{platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE, readiness.ProviderCancellationCooperative},
		{platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_BOUNDED, readiness.ProviderCancellationBounded},
	}
	for _, mapping := range cancellationMappings {
		profile := validOperationalProfile()
		profile.CancellationSemantics = mapping.wire
		got, err := normalizeOperationalProfile(profile)
		if err != nil || got.CancellationSemantics != mapping.app {
			t.Fatalf("normalizeOperationalProfile(%s) cancellation = %q, %v, want %q", mapping.wire, got.CancellationSemantics, err, mapping.app)
		}
	}

	profile := validOperationalProfile()
	profile.DeviceRequirements = []platformv1.ProviderDeviceClass{
		platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_EMBODIMENT_CONTROLLER,
		platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_AUDIO_OUTPUT,
		platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_DISPLAY,
		platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE,
		platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA,
	}
	got, err := normalizeOperationalProfile(profile)
	wantDevices := []readiness.ProviderDeviceClass{
		readiness.ProviderDeviceCamera,
		readiness.ProviderDeviceMicrophone,
		readiness.ProviderDeviceDisplay,
		readiness.ProviderDeviceAudioOutput,
		readiness.ProviderDeviceEmbodimentController,
	}
	if err != nil || !reflect.DeepEqual(got.DeviceRequirements, wantDevices) {
		t.Fatalf("normalizeOperationalProfile() devices = %#v, %v, want %#v", got.DeviceRequirements, err, wantDevices)
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
		{name: "missing operational profile", request: registrationWith(func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.OperationalProfile = nil
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

func TestRegisterMapsOperationalProfileAndReturnsIndependentSnapshots(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	_, updates, cancel := registry.Subscribe()
	defer cancel()

	request := validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
	request.OperationalProfile = validOperationalProfile()
	registration := registerOK(t, registry, request)

	want := readiness.ProviderOperationalProfile{
		PrivacyClass:          readiness.ProviderPrivacyDeviceLocal,
		MaximumLatency:        250 * time.Millisecond,
		CancellationSemantics: readiness.ProviderCancellationBounded,
		DeviceRequirements: []readiness.ProviderDeviceClass{
			readiness.ProviderDeviceCamera,
			readiness.ProviderDeviceMicrophone,
		},
	}
	update := receiveSnapshots(t, updates)
	if len(update) != 1 || !reflect.DeepEqual(update[0].OperationalProfile, want) {
		t.Fatalf("subscription profile = %#v, want %#v", update, want)
	}
	update[0].OperationalProfile.DeviceRequirements[0] = readiness.ProviderDeviceDisplay

	snapshots := registry.Snapshots()
	if len(snapshots) != 1 || !reflect.DeepEqual(snapshots[0].OperationalProfile, want) {
		t.Fatalf("Snapshots() profile = %#v, want %#v", snapshots, want)
	}
	snapshots[0].OperationalProfile.DeviceRequirements[0] = readiness.ProviderDeviceDisplay

	lease, ok := registry.LeaseSnapshot(registration.LeaseId)
	if !ok || !reflect.DeepEqual(lease.OperationalProfile, want) {
		t.Fatalf("LeaseSnapshot() profile = %#v, %t, want %#v", lease.OperationalProfile, ok, want)
	}
	lease.OperationalProfile.DeviceRequirements[0] = readiness.ProviderDeviceDisplay
	if current := registry.Snapshots()[0].OperationalProfile; !reflect.DeepEqual(current, want) {
		t.Fatalf("returned profile mutation changed registry snapshot: %#v", current)
	}

	registerOK(t, registry, request)
	if current := registry.Snapshots()[0].OperationalProfile; !reflect.DeepEqual(current, want) {
		t.Fatalf("idempotent register changed profile: %#v", current)
	}
	if _, err := registry.HeartbeatCapabilityProvider(context.Background(), healthyHeartbeat(registration.LeaseId)); err != nil {
		t.Fatalf("HeartbeatCapabilityProvider() error = %v", err)
	}
	if current := registry.Snapshots()[0].OperationalProfile; !reflect.DeepEqual(current, want) {
		t.Fatalf("heartbeat changed profile: %#v", current)
	}
}

func TestRegisterRejectsInvalidOperationalProfiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*platformv1.ProviderOperationalProfile)
	}{
		{name: "unspecified privacy", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.PrivacyClass = platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_UNSPECIFIED
		}},
		{name: "unknown privacy", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.PrivacyClass = platformv1.ProviderPrivacyClass(99)
		}},
		{name: "missing maximum latency", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.MaximumLatency = nil
		}},
		{name: "zero maximum latency", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.MaximumLatency = durationpb.New(0)
		}},
		{name: "negative maximum latency", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.MaximumLatency = durationpb.New(-time.Nanosecond)
		}},
		{name: "out of range maximum latency", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.MaximumLatency = &durationpb.Duration{Seconds: 315576000001}
		}},
		{name: "unspecified cancellation", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.CancellationSemantics = platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_UNSPECIFIED
		}},
		{name: "unknown cancellation", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.CancellationSemantics = platformv1.ProviderCancellationSemantics(99)
		}},
		{name: "unspecified device", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.DeviceRequirements = []platformv1.ProviderDeviceClass{platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_UNSPECIFIED}
		}},
		{name: "unknown device", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.DeviceRequirements = []platformv1.ProviderDeviceClass{platformv1.ProviderDeviceClass(99)}
		}},
		{name: "duplicate device", mutate: func(profile *platformv1.ProviderOperationalProfile) {
			profile.DeviceRequirements = []platformv1.ProviderDeviceClass{
				platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA,
				platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA,
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
			request := validRegistration("provider", "instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE)
			request.OperationalProfile = validOperationalProfile()
			test.mutate(request.OperationalProfile)
			_, err := registry.RegisterCapabilityProvider(context.Background(), request)
			requireStatusCode(t, err, codes.InvalidArgument)
			if snapshots := registry.Snapshots(); len(snapshots) != 0 {
				t.Fatalf("invalid profile mutated snapshots: %#v", snapshots)
			}
		})
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
		{name: "changed operational profile", mutate: func(r *platformv1.RegisterCapabilityProviderRequest) {
			r.OperationalProfile.PrivacyClass = platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_REMOTE_PROCESSING
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

func TestSubscribePublishesImmutableLatestSnapshotsForSuccessfulMutations(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, _ := New(clock, time.Minute)
	initial, updates, cancel := registry.Subscribe()
	defer cancel()
	if len(initial) != 0 {
		t.Fatalf("initial snapshots = %#v, want empty", initial)
	}

	registration := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
	registered := receiveSnapshots(t, updates)
	if len(registered) != 1 || registered[0].ProviderID != "camera" {
		t.Fatalf("registered snapshots = %#v", registered)
	}
	registered[0].Capabilities[0] = readiness.DeviceState
	if current := registry.Snapshots(); current[0].Capabilities[0] != readiness.PersonPresence {
		t.Fatalf("subscriber mutated registry snapshot = %#v", current)
	}

	if _, err := registry.HeartbeatCapabilityProvider(context.Background(), healthyHeartbeat(registration.LeaseId)); err != nil {
		t.Fatalf("HeartbeatCapabilityProvider() error = %v", err)
	}
	heartbeat := receiveSnapshots(t, updates)
	if len(heartbeat) != 1 || heartbeat[0].Health != readiness.Healthy {
		t.Fatalf("heartbeat snapshots = %#v", heartbeat)
	}

	if _, err := registry.UnregisterCapabilityProvider(context.Background(), &platformv1.UnregisterCapabilityProviderRequest{LeaseId: registration.LeaseId}); err != nil {
		t.Fatalf("UnregisterCapabilityProvider() error = %v", err)
	}
	if unregistered := receiveSnapshots(t, updates); len(unregistered) != 0 {
		t.Fatalf("unregistered snapshots = %#v, want empty", unregistered)
	}
}

func TestRevokeProviderRemovesOnlyTheOwnedLocalProvider(t *testing.T) {
	registry, _ := New(engineclock.NewFake(testNow()), time.Minute)
	camera := registerOK(t, registry, validRegistration("camera", "camera-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE))
	registerOK(t, registry, validRegistration("vad", "vad-1", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY))

	if !registry.RevokeProvider("camera") {
		t.Fatal("RevokeProvider(camera) = false, want true")
	}
	if registry.RevokeProvider("camera") || registry.RevokeProvider("") {
		t.Fatal("repeated or empty RevokeProvider() unexpectedly removed a provider")
	}
	if _, exists := registry.LeaseSnapshot(camera.LeaseId); exists {
		t.Fatal("revoked camera lease remains available")
	}
	snapshots := registry.Snapshots()
	if len(snapshots) != 1 || snapshots[0].ProviderID != "vad" {
		t.Fatalf("snapshots after revoke = %#v, want only vad", snapshots)
	}
}

func receiveSnapshots(t *testing.T, updates <-chan []readiness.ProviderSnapshot) []readiness.ProviderSnapshot {
	t.Helper()
	select {
	case snapshots, ok := <-updates:
		if !ok {
			t.Fatal("snapshot subscription closed")
		}
		return snapshots
	case <-time.After(time.Second):
		t.Fatal("snapshot update not published")
		return nil
	}
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
		OperationalProfile:    validOperationalProfile(),
	}
}

func validOperationalProfile() *platformv1.ProviderOperationalProfile {
	return &platformv1.ProviderOperationalProfile{
		PrivacyClass:          platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
		MaximumLatency:        durationpb.New(250 * time.Millisecond),
		CancellationSemantics: platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_BOUNDED,
		DeviceRequirements: []platformv1.ProviderDeviceClass{
			platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE,
			platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA,
		},
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
