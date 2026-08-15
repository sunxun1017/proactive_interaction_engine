package control

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	_ ProviderLeaseReader = (*controlLeaseReader)(nil)
	_ PermissionReader    = (*controlPermissionReader)(nil)
)

func TestVisionWatchPublishesPresenceAwareWorkAndClears(t *testing.T) {
	server, leases, permissions, clock := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	stream := newVisionStream(ctx)
	done := make(chan error, 1)
	go func() { done <- server.WatchVision(validVisionWatch(), stream) }()

	initial := stream.next(t)
	if initial.GetRevision() != 1 || initial.GetIdle() == nil || initial.GetActive() != nil {
		t.Fatalf("initial vision snapshot = %#v, want revision 1 idle", initial)
	}

	templateBytes := []byte("face-template")
	work := VisionWork{
		EvidenceWindowID: "window-1",
		OpenedAt:         clock.Now().Add(-time.Second),
		Deadline:         clock.Now().Add(time.Second),
		Detection:        &FaceDetectionTask{},
		Identification: &FaceIdentificationTask{Templates: []IdentificationTemplate{{
			ProfileRef: "profile-a", Encoded: templateBytes,
		}}},
		Liveness: &FaceLivenessTask{},
	}
	if err := server.PublishVision(context.Background(), "vision-instance", work); err != nil {
		t.Fatalf("PublishVision() error = %v", err)
	}
	templateBytes[0] = 'X'
	active := stream.next(t)
	if active.GetRevision() != 2 || active.GetActive() == nil || active.GetIdle() != nil {
		t.Fatalf("active vision snapshot = %#v", active)
	}
	wire := active.GetActive()
	if wire.GetEvidenceWindowId() != "window-1" || wire.GetDetection() == nil || wire.GetIdentification() == nil || wire.GetLiveness() == nil {
		t.Fatalf("active vision work = %#v", wire)
	}
	if got := wire.GetIdentification().GetTemplates()[0]; got.GetProfileRef() != "profile-a" || string(got.GetEncodedTemplate()) != "face-template" {
		t.Fatalf("identification template = %#v, want cloned material", got)
	}
	if permissions.calls() != 1 || leases.calls() != 8 {
		t.Fatalf("dependency calls permissions=%d leases=%d, want 1 and 8 (watch+publish)", permissions.calls(), leases.calls())
	}

	beforeLeaseCalls, beforePermissionCalls := leases.calls(), permissions.calls()
	if err := server.ClearVision("vision-instance"); err != nil {
		t.Fatalf("ClearVision() error = %v", err)
	}
	idle := stream.next(t)
	if idle.GetRevision() != 3 || idle.GetIdle() == nil || idle.GetActive() != nil {
		t.Fatalf("cleared vision snapshot = %#v, want revision 3 idle", idle)
	}
	if err := server.ClearVision("vision-instance"); err != nil {
		t.Fatalf("idempotent ClearVision() error = %v", err)
	}
	if leases.calls() != beforeLeaseCalls || permissions.calls() != beforePermissionCalls {
		t.Fatal("idle clear read lease or privacy state")
	}

	cancel()
	if err := waitStream(t, done); status.Code(err) != codes.Canceled {
		t.Fatalf("WatchVision() code = %s, want Canceled", status.Code(err))
	}
	for _, raw := range stream.rawSnapshots() {
		if identification := raw.GetActive().GetIdentification(); identification != nil {
			for _, template := range identification.GetTemplates() {
				if len(template.GetEncodedTemplate()) != 0 {
					t.Fatal("server retained vision template bytes after Send returned")
				}
			}
		}
	}
	server.mu.Lock()
	_, retained := server.visionWatchers["vision-instance"]
	server.mu.Unlock()
	if retained {
		t.Fatal("server retained disconnected vision watcher")
	}
}

func TestAudioWatchPublishesStrictIdentificationAndVerificationOneof(t *testing.T) {
	server, _, _, clock := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	stream := newAudioStream(ctx)
	done := make(chan error, 1)
	go func() { done <- server.WatchAudio(validAudioWatch(), stream) }()
	if initial := stream.next(t); initial.GetRevision() != 1 || initial.GetIdle() == nil {
		t.Fatalf("initial audio snapshot = %#v", initial)
	}

	identification := AudioWork{Identification: &SpeakerIdentificationWork{
		EvidenceWindowID: "window-audio", OpenedAt: clock.Now().Add(-time.Second), Deadline: clock.Now().Add(time.Second),
		Templates: []IdentificationTemplate{{ProfileRef: "profile-a", Encoded: []byte("speaker-template")}},
	}}
	if err := server.PublishAudio(context.Background(), "audio-instance", identification); err != nil {
		t.Fatalf("PublishAudio(identification) error = %v", err)
	}
	identified := stream.next(t)
	if identified.GetRevision() != 2 || identified.GetActive().GetIdentification() == nil || identified.GetActive().GetVerification() != nil {
		t.Fatalf("speaker identification snapshot = %#v", identified)
	}

	verification := AudioWork{Verification: &SpeakerVerificationWork{
		ChallengeID: "challenge-1", EvidenceWindowID: "verification-window-1",
		OpenedAt: clock.Now().Add(-time.Second), Deadline: clock.Now().Add(time.Second),
		ExpectedEncoded: []byte("expected-speaker-template"),
	}}
	if err := server.PublishAudio(context.Background(), "audio-instance", verification); err != nil {
		t.Fatalf("PublishAudio(verification) error = %v", err)
	}
	verified := stream.next(t)
	verificationWire := verified.GetActive().GetVerification()
	if verified.GetRevision() != 3 || verificationWire == nil || verified.GetActive().GetIdentification() != nil {
		t.Fatalf("speaker verification snapshot = %#v", verified)
	}
	if verificationWire.GetVerificationChallengeId() != "challenge-1" || verificationWire.GetEvidenceWindowId() != "verification-window-1" || string(verificationWire.GetExpectedEncodedTemplate()) != "expected-speaker-template" {
		t.Fatalf("speaker verification work = %#v", verificationWire)
	}
	serialized, err := proto.Marshal(verified)
	if err != nil {
		t.Fatalf("marshal verification: %v", err)
	}
	for _, forbidden := range [][]byte{[]byte("profile-a"), []byte("template-ref"), []byte("expected-profile")} {
		if containsBytes(serialized, forbidden) {
			t.Fatalf("verification wire leaked forbidden profile material %q", forbidden)
		}
	}

	cancel()
	if err := waitStream(t, done); status.Code(err) != codes.Canceled {
		t.Fatalf("WatchAudio() code = %s, want Canceled", status.Code(err))
	}
	for _, raw := range stream.rawSnapshots() {
		if identification := raw.GetActive().GetIdentification(); identification != nil {
			for _, template := range identification.GetTemplates() {
				if len(template.GetEncodedTemplate()) != 0 {
					t.Fatal("server retained speaker identification bytes after Send returned")
				}
			}
		}
		if verification := raw.GetActive().GetVerification(); verification != nil && len(verification.GetExpectedEncodedTemplate()) != 0 {
			t.Fatal("server retained speaker verification bytes after Send returned")
		}
	}
}

func TestWatchRequestsRequireOneHealthySelectedSingleCapabilityLeasePerBinding(t *testing.T) {
	tests := []struct {
		name    string
		request *platformv1.WatchVisionRequest
		mutate  func(*controlLeaseReader)
		code    codes.Code
	}{
		{name: "missing presence", request: &platformv1.WatchVisionRequest{SourceInstanceId: "vision-instance", LeaseBindings: validVisionWatch().GetLeaseBindings()[1:]}, code: codes.InvalidArgument},
		{name: "wrong stack capability", request: func() *platformv1.WatchVisionRequest {
			r := validVisionWatch()
			r.LeaseBindings[1].Capability = platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION
			return r
		}(), code: codes.InvalidArgument},
		{name: "duplicated capability", request: func() *platformv1.WatchVisionRequest {
			r := validVisionWatch()
			r.LeaseBindings = append(r.LeaseBindings, proto.Clone(r.LeaseBindings[0]).(*platformv1.IdentityWorkerLeaseBinding))
			return r
		}(), code: codes.InvalidArgument},
		{name: "duplicated lease id", request: func() *platformv1.WatchVisionRequest {
			r := validVisionWatch()
			r.LeaseBindings[1].ProviderLeaseId = r.LeaseBindings[0].GetProviderLeaseId()
			return r
		}(), code: codes.InvalidArgument},
		{name: "missing lease", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.remove("lease-presence")
		}, code: codes.PermissionDenied},
		{name: "lease declares multiple capabilities", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.update("lease-presence", func(p *readiness.ProviderSnapshot) { p.Capabilities = append(p.Capabilities, readiness.FaceDetection) })
		}, code: codes.PermissionDenied},
		{name: "unhealthy", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.update("lease-presence", func(p *readiness.ProviderSnapshot) {
				p.Health = readiness.Unhealthy
				p.HealthReason = readiness.ProviderHealthReasonDeviceUnavailable
			})
		}, code: codes.PermissionDenied},
		{name: "exact expiry", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.update("lease-presence", func(p *readiness.ProviderSnapshot) { p.LeaseExpiresAt = controlNow() })
		}, code: codes.PermissionDenied},
		{name: "wrong instance", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.update("lease-presence", func(p *readiness.ProviderSnapshot) { p.InstanceID = "other-instance" })
		}, code: codes.PermissionDenied},
		{name: "not selected", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.update("lease-presence", func(p *readiness.ProviderSnapshot) { p.ProviderID = "other-provider" })
		}, code: codes.PermissionDenied},
		{name: "operationally incompatible", request: validVisionWatch(), mutate: func(leases *controlLeaseReader) {
			leases.update("lease-presence", func(p *readiness.ProviderSnapshot) { p.ProtocolVersion = "v2" })
		}, code: codes.PermissionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, leases, _, _ := newControlTestServer(t)
			if test.mutate != nil {
				test.mutate(leases)
			}
			stream := newVisionStream(context.Background())
			if err := server.WatchVision(test.request, stream); status.Code(err) != test.code {
				t.Fatalf("WatchVision() code = %s, want %s (error %v)", status.Code(err), test.code, err)
			}
		})
	}
}

func TestAudioWatchRequiresVADAndRejectsVisionCapabilities(t *testing.T) {
	server, _, _, _ := newControlTestServer(t)
	missingVAD := validAudioWatch()
	missingVAD.LeaseBindings = missingVAD.LeaseBindings[1:]
	if err := server.WatchAudio(missingVAD, newAudioStream(context.Background())); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("WatchAudio(missing VAD) code = %s, want InvalidArgument", status.Code(err))
	}
	wrongStack := validAudioWatch()
	wrongStack.LeaseBindings[1].Capability = platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION
	if err := server.WatchAudio(wrongStack, newAudioStream(context.Background())); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("WatchAudio(vision capability) code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestDuplicateWatcherRejectedAndReconnectGetsOnlyIdle(t *testing.T) {
	server, _, _, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := newVisionStream(ctx)
	done := make(chan error, 1)
	go func() { done <- server.WatchVision(validVisionWatch(), first) }()
	_ = first.next(t)

	if err := server.WatchVision(validVisionWatch(), newVisionStream(context.Background())); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate WatchVision() code = %s, want AlreadyExists", status.Code(err))
	}
	cancel()
	_ = waitStream(t, done)

	reconnectCtx, reconnectCancel := context.WithCancel(context.Background())
	reconnect := newVisionStream(reconnectCtx)
	reconnectDone := make(chan error, 1)
	go func() { reconnectDone <- server.WatchVision(validVisionWatch(), reconnect) }()
	if snapshot := reconnect.next(t); snapshot.GetRevision() != 1 || snapshot.GetIdle() == nil || snapshot.GetActive() != nil {
		t.Fatalf("reconnected initial snapshot = %#v, want fresh idle without replay", snapshot)
	}
	reconnectCancel()
	_ = waitStream(t, reconnectDone)
}

func TestActivePublishRevalidatesHalfOpenWindowLeaseAndPermission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*controlLeaseReader, *controlPermissionReader, *engineclock.Fake, *VisionWork)
		code   fault.Code
	}{
		{name: "exact deadline", mutate: func(_ *controlLeaseReader, _ *controlPermissionReader, clock *engineclock.Fake, work *VisionWork) {
			work.OpenedAt, work.Deadline = clock.Now().Add(-time.Second), clock.Now()
		}, code: fault.StaleInput},
		{name: "before open", mutate: func(_ *controlLeaseReader, _ *controlPermissionReader, clock *engineclock.Fake, work *VisionWork) {
			work.OpenedAt = clock.Now().Add(time.Nanosecond)
		}, code: fault.InvalidInput},
		{name: "lease expired", mutate: func(leases *controlLeaseReader, _ *controlPermissionReader, clock *engineclock.Fake, _ *VisionWork) {
			leases.update("lease-detection", func(p *readiness.ProviderSnapshot) { p.LeaseExpiresAt = clock.Now() })
		}, code: fault.PermissionDenied},
		{name: "permission revoked", mutate: func(_ *controlLeaseReader, permissions *controlPermissionReader, _ *engineclock.Fake, _ *VisionWork) {
			permissions.disable(privacy.FaceDetection)
		}, code: fault.PermissionDenied},
		{name: "permission dependency unavailable", mutate: func(_ *controlLeaseReader, permissions *controlPermissionReader, _ *engineclock.Fake, _ *VisionWork) {
			permissions.setError(fault.New(fault.Unavailable, "test", errors.New("offline")))
		}, code: fault.Unavailable},
		{name: "permission dependency deadline", mutate: func(_ *controlLeaseReader, permissions *controlPermissionReader, _ *engineclock.Fake, _ *VisionWork) {
			permissions.setError(fault.New(fault.DeadlineExceeded, "test", context.DeadlineExceeded))
		}, code: fault.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, leases, permissions, clock := newControlTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			stream := newVisionStream(ctx)
			done := make(chan error, 1)
			go func() { done <- server.WatchVision(validVisionWatch(), stream) }()
			_ = stream.next(t)
			work := VisionWork{EvidenceWindowID: "window", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{}}
			test.mutate(leases, permissions, clock, &work)
			if err := server.PublishVision(context.Background(), "vision-instance", work); !fault.IsCode(err, test.code) {
				t.Fatalf("PublishVision() error = %v, want %s", err, test.code)
			}
			cancel()
			_ = waitStream(t, done)
		})
	}
}

func TestPublishContextFaultsPreserveTypedCategory(t *testing.T) {
	server, _, _, clock := newControlTestServer(t)
	work := VisionWork{EvidenceWindowID: "window", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{}}
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDeadline()
	if err := server.PublishVision(deadline, "vision-instance", work); !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("PublishVision(deadline context) error = %v, want DeadlineExceeded", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.PublishVision(canceled, "vision-instance", work); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("PublishVision(canceled context) error = %v, want Unavailable", err)
	}
}

func TestActivePublishIgnoresUnrelatedOptionalLeaseFailure(t *testing.T) {
	t.Run("detection ignores face identification and liveness", func(t *testing.T) {
		server, leases, _, clock := newControlTestServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		stream := newVisionStream(ctx)
		done := make(chan error, 1)
		go func() { done <- server.WatchVision(validVisionWatch(), stream) }()
		_ = stream.next(t)
		leases.update("lease-identification", func(p *readiness.ProviderSnapshot) {
			p.Health, p.HealthReason = readiness.Unhealthy, readiness.ProviderHealthReasonModelUnavailable
		})
		leases.update("lease-liveness", func(p *readiness.ProviderSnapshot) { p.LeaseExpiresAt = clock.Now() })
		if err := server.PublishVision(context.Background(), "vision-instance", VisionWork{
			EvidenceWindowID: "detection-window", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{},
		}); err != nil {
			t.Fatalf("PublishVision(detection-only) error = %v", err)
		}
		if snapshot := stream.next(t); snapshot.GetActive().GetDetection() == nil || snapshot.GetActive().GetIdentification() != nil || snapshot.GetActive().GetLiveness() != nil {
			t.Fatalf("detection-only snapshot = %#v", snapshot)
		}
		cancel()
		_ = waitStream(t, done)
	})

	for _, test := range []struct {
		name        string
		failedLease string
		work        func(time.Time) AudioWork
		assert      func(*platformv1.AudioActiveWork) bool
	}{
		{
			name: "speaker identification ignores verification", failedLease: "lease-speaker-verification",
			work: func(now time.Time) AudioWork {
				return AudioWork{Identification: &SpeakerIdentificationWork{EvidenceWindowID: "identification-window", OpenedAt: now, Deadline: now.Add(time.Second)}}
			},
			assert: func(active *platformv1.AudioActiveWork) bool {
				return active.GetIdentification() != nil && active.GetVerification() == nil
			},
		},
		{
			name: "speaker verification ignores identification", failedLease: "lease-speaker-identification",
			work: func(now time.Time) AudioWork {
				return AudioWork{Verification: &SpeakerVerificationWork{ChallengeID: "challenge", EvidenceWindowID: "verification-window", OpenedAt: now, Deadline: now.Add(time.Second), ExpectedEncoded: []byte("expected")}}
			},
			assert: func(active *platformv1.AudioActiveWork) bool {
				return active.GetVerification() != nil && active.GetIdentification() == nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, leases, _, clock := newControlTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			stream := newAudioStream(ctx)
			done := make(chan error, 1)
			go func() { done <- server.WatchAudio(validAudioWatch(), stream) }()
			_ = stream.next(t)
			leases.update(test.failedLease, func(p *readiness.ProviderSnapshot) {
				p.Health, p.HealthReason = readiness.Unhealthy, readiness.ProviderHealthReasonModelUnavailable
			})
			if err := server.PublishAudio(context.Background(), "audio-instance", test.work(clock.Now())); err != nil {
				t.Fatalf("PublishAudio() error = %v", err)
			}
			if snapshot := stream.next(t); snapshot.GetActive() == nil || !test.assert(snapshot.GetActive()) {
				t.Fatalf("audio snapshot = %#v", snapshot)
			}
			cancel()
			_ = waitStream(t, done)
		})
	}
}

func TestLimitsStrictOneofsAndTaskDependencies(t *testing.T) {
	server, _, _, clock := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	vision, audio := newVisionStream(ctx), newAudioStream(ctx)
	visionDone, audioDone := make(chan error, 1), make(chan error, 1)
	go func() { visionDone <- server.WatchVision(validVisionWatch(), vision) }()
	go func() { audioDone <- server.WatchAudio(validAudioWatch(), audio) }()
	_ = vision.next(t)
	_ = audio.next(t)

	windowOpen, windowDeadline := clock.Now(), clock.Now().Add(time.Second)
	visionTests := []VisionWork{
		{EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline},
		{EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline, Identification: &FaceIdentificationTask{}},
		{EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline, Liveness: &FaceLivenessTask{}},
		{EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline, Detection: &FaceDetectionTask{}, Identification: &FaceIdentificationTask{Templates: []IdentificationTemplate{{ProfileRef: "profile-a", Encoded: make([]byte, 65)}}}},
	}
	for index, work := range visionTests {
		if err := server.PublishVision(context.Background(), "vision-instance", work); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("PublishVision(invalid %d) error = %v, want InvalidInput", index, err)
		}
	}

	audioTests := []AudioWork{
		{},
		{Identification: &SpeakerIdentificationWork{EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline}, Verification: &SpeakerVerificationWork{ChallengeID: "challenge", EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline, ExpectedEncoded: []byte("x")}},
		{Verification: &SpeakerVerificationWork{ChallengeID: "challenge", EvidenceWindowID: "window", OpenedAt: windowOpen, Deadline: windowDeadline}},
	}
	for index, work := range audioTests {
		if err := server.PublishAudio(context.Background(), "audio-instance", work); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("PublishAudio(invalid %d) error = %v, want InvalidInput", index, err)
		}
	}

	cancel()
	_ = waitStream(t, visionDone)
	_ = waitStream(t, audioDone)
}

func TestLatestQueuedStateReplacesAndZeroesOverwrittenMaterial(t *testing.T) {
	server, _, _, clock := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	stream := newBlockingVisionStream(ctx)
	done := make(chan error, 1)
	go func() { done <- server.WatchVision(validVisionWatch(), stream) }()
	_ = stream.next(t)

	first := VisionWork{EvidenceWindowID: "window-1", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{}}
	if err := server.PublishVision(context.Background(), "vision-instance", first); err != nil {
		t.Fatalf("PublishVision(first) error = %v", err)
	}
	stream.waitBlocked(t)

	secondBytes := []byte("12345678")
	second := VisionWork{EvidenceWindowID: "window-2", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{}, Identification: &FaceIdentificationTask{Templates: []IdentificationTemplate{{ProfileRef: "profile-b", Encoded: secondBytes}}}}
	if err := server.PublishVision(context.Background(), "vision-instance", second); err != nil {
		t.Fatalf("PublishVision(second) error = %v", err)
	}

	server.mu.Lock()
	watcher := server.visionWatchers["vision-instance"]
	overwritten := <-watcher.updates
	overwrittenBytes := overwritten.GetActive().GetIdentification().GetTemplates()[0].GetEncodedTemplate()
	watcher.updates <- overwritten
	server.mu.Unlock()

	third := VisionWork{EvidenceWindowID: "window-3", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{}}
	if err := server.PublishVision(context.Background(), "vision-instance", third); err != nil {
		t.Fatalf("PublishVision(third) error = %v", err)
	}
	for _, value := range overwrittenBytes {
		if value != 0 {
			t.Fatal("overwritten queued template bytes were not cleared")
		}
	}
	if string(secondBytes) != "12345678" {
		t.Fatal("server cleared caller-owned template bytes instead of its clone")
	}

	stream.release()
	if sent := stream.next(t); sent.GetRevision() != 2 || sent.GetActive().GetEvidenceWindowId() != "window-1" {
		t.Fatalf("already-sending snapshot = %#v, want revision 2 window-1", sent)
	}
	if sent := stream.next(t); sent.GetRevision() != 4 || sent.GetActive().GetEvidenceWindowId() != "window-3" {
		t.Fatalf("latest delivered snapshot = %#v, want revision 4 window-3", sent)
	}
	cancel()
	_ = waitStream(t, done)
}

func TestRevisionOverflowTerminatesWatcherFailClosed(t *testing.T) {
	server, _, _, clock := newControlTestServer(t)
	ctx := context.Background()
	stream := newVisionStream(ctx)
	done := make(chan error, 1)
	go func() { done <- server.WatchVision(validVisionWatch(), stream) }()
	_ = stream.next(t)

	server.mu.Lock()
	server.visionWatchers["vision-instance"].revision = math.MaxUint64
	server.mu.Unlock()
	err := server.PublishVision(context.Background(), "vision-instance", VisionWork{EvidenceWindowID: "window", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{}})
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("PublishVision(revision overflow) error = %v, want Unavailable", err)
	}
	if streamErr := waitStream(t, done); status.Code(streamErr) != codes.ResourceExhausted {
		t.Fatalf("WatchVision(revision overflow) code = %s, want ResourceExhausted", status.Code(streamErr))
	}
}

func TestNewServerRejectsInvalidDependenciesAndLimits(t *testing.T) {
	now := controlNow()
	leases, permissions, clock, scenario := controlLeases(now), allControlPermissions(now), engineclock.NewFake(now), controlScenario()
	valid := Config{MaxWatchers: 4, MaxTemplatesPerWork: 4, MaxTemplateBytes: 64, MaxTotalTemplateBytes: 128}
	if server, err := NewServer(valid, leases, permissions, clock, scenario); err != nil || server == nil {
		t.Fatalf("NewServer(valid) = %#v, %v", server, err)
	}
	invalidConfigs := []Config{
		{},
		{MaxWatchers: 0, MaxTemplatesPerWork: 4, MaxTemplateBytes: 64, MaxTotalTemplateBytes: 128},
		{MaxWatchers: 4, MaxTemplatesPerWork: 0, MaxTemplateBytes: 64, MaxTotalTemplateBytes: 128},
		{MaxWatchers: 4, MaxTemplatesPerWork: 4, MaxTemplateBytes: 0, MaxTotalTemplateBytes: 128},
		{MaxWatchers: 4, MaxTemplatesPerWork: 4, MaxTemplateBytes: 64, MaxTotalTemplateBytes: 63},
	}
	for _, config := range invalidConfigs {
		if server, err := NewServer(config, leases, permissions, clock, scenario); err == nil || server != nil {
			t.Fatalf("NewServer(%#v) = %#v, %v; want failure", config, server, err)
		}
	}
	if server, err := NewServer(valid, nil, permissions, clock, scenario); err == nil || server != nil {
		t.Fatalf("NewServer(nil leases) = %#v, %v; want failure", server, err)
	}
}

func newControlTestServer(t *testing.T) (*Server, *controlLeaseReader, *controlPermissionReader, *engineclock.Fake) {
	t.Helper()
	now := controlNow()
	leases, permissions, clock := controlLeases(now), allControlPermissions(now), engineclock.NewFake(now)
	server, err := NewServer(Config{MaxWatchers: 4, MaxTemplatesPerWork: 4, MaxTemplateBytes: 64, MaxTotalTemplateBytes: 128}, leases, permissions, clock, controlScenario())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server, leases, permissions, clock
}

func controlNow() time.Time { return time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC) }

func controlScenario() readiness.ScenarioRequirements {
	capabilities := []readiness.CapabilityKind{
		readiness.PersonPresence, readiness.FaceDetection, readiness.FaceIdentification, readiness.FaceLiveness,
		readiness.VoiceActivity, readiness.SpeakerIdentification, readiness.SpeakerVerification,
	}
	scenario := readiness.ScenarioRequirements{ID: "identity-control", MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous}
	for _, capability := range capabilities {
		scenario.Required = append(scenario.Required, readiness.CapabilityRequirement{Kind: capability, ProviderID: controlProviderID(capability), Compatibility: controlCompatibility(capability)})
	}
	return scenario
}

func controlCompatibility(capability readiness.CapabilityKind) readiness.ProviderCompatibility {
	device := readiness.ProviderDeviceCamera
	if capability == readiness.VoiceActivity || capability == readiness.SpeakerIdentification || capability == readiness.SpeakerVerification {
		device = readiness.ProviderDeviceMicrophone
	}
	return readiness.ProviderCompatibility{
		ProtocolVersion: "v1", AllowedPrivacyClasses: []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal}, MaximumLatency: time.Second,
		AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
		AllowedDeviceClasses:         []readiness.ProviderDeviceClass{device},
	}
}

func controlProviderID(capability readiness.CapabilityKind) string {
	return "provider-" + string(capability)
}

func controlLeases(now time.Time) *controlLeaseReader {
	snapshots := make(map[string]readiness.ProviderSnapshot)
	for _, binding := range append(validVisionWatch().GetLeaseBindings(), validAudioWatch().GetLeaseBindings()...) {
		capability, _ := controlCapabilityFromWire(binding.GetCapability())
		instance := "vision-instance"
		device := readiness.ProviderDeviceCamera
		if capability == readiness.VoiceActivity || capability == readiness.SpeakerIdentification || capability == readiness.SpeakerVerification {
			instance, device = "audio-instance", readiness.ProviderDeviceMicrophone
		}
		snapshots[binding.GetProviderLeaseId()] = readiness.ProviderSnapshot{
			ProviderID: controlProviderID(capability), InstanceID: instance, ProtocolVersion: "v1", ImplementationVersion: "test.v1",
			Capabilities: []readiness.CapabilityKind{capability}, Health: readiness.Healthy, HealthReason: readiness.ProviderHealthReasonNone, LeaseExpiresAt: now.Add(time.Minute),
			OperationalProfile: readiness.ProviderOperationalProfile{PrivacyClass: readiness.ProviderPrivacyDeviceLocal, MaximumLatency: 100 * time.Millisecond, CancellationSemantics: readiness.ProviderCancellationCooperative, DeviceRequirements: []readiness.ProviderDeviceClass{device}},
		}
	}
	return &controlLeaseReader{snapshots: snapshots}
}

func validVisionWatch() *platformv1.WatchVisionRequest {
	return &platformv1.WatchVisionRequest{SourceInstanceId: "vision-instance", LeaseBindings: []*platformv1.IdentityWorkerLeaseBinding{
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE, ProviderLeaseId: "lease-presence"},
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION, ProviderLeaseId: "lease-detection"},
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION, ProviderLeaseId: "lease-identification"},
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_LIVENESS, ProviderLeaseId: "lease-liveness"},
	}}
}

func validAudioWatch() *platformv1.WatchAudioRequest {
	return &platformv1.WatchAudioRequest{SourceInstanceId: "audio-instance", LeaseBindings: []*platformv1.IdentityWorkerLeaseBinding{
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY, ProviderLeaseId: "lease-vad"},
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION, ProviderLeaseId: "lease-speaker-identification"},
		{Capability: platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION, ProviderLeaseId: "lease-speaker-verification"},
	}}
}

func allControlPermissions(now time.Time) *controlPermissionReader {
	snapshot := privacy.Snapshot{Revision: 1}
	for _, permission := range privacy.AllPermissions() {
		snapshot.Grants = append(snapshot.Grants, privacy.Grant{Permission: permission, Enabled: true, UpdatedAt: now})
	}
	return &controlPermissionReader{snapshot: snapshot}
}

type controlLeaseReader struct {
	mu        sync.Mutex
	snapshots map[string]readiness.ProviderSnapshot
	reads     int
}

func (r *controlLeaseReader) LeaseSnapshot(id string) (readiness.ProviderSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	snapshot, ok := r.snapshots[id]
	return snapshot, ok
}

func (r *controlLeaseReader) update(id string, mutate func(*readiness.ProviderSnapshot)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := r.snapshots[id]
	mutate(&snapshot)
	r.snapshots[id] = snapshot
}

func (r *controlLeaseReader) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.snapshots, id)
}

func (r *controlLeaseReader) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

type controlPermissionReader struct {
	mu       sync.Mutex
	snapshot privacy.Snapshot
	err      error
	reads    int
}

func (r *controlPermissionReader) Current(context.Context) (privacy.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return r.snapshot, r.err
}

func (r *controlPermissionReader) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func (r *controlPermissionReader) disable(permission privacy.Permission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.snapshot.Grants {
		if r.snapshot.Grants[index].Permission == permission {
			r.snapshot.Grants[index].Enabled = false
		}
	}
}

func (r *controlPermissionReader) setError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

type visionStream struct {
	ctx         context.Context
	sent        chan *platformv1.WatchVisionResponse
	blocked     chan struct{}
	releaseSend chan struct{}
	rawMu       sync.Mutex
	raw         []*platformv1.WatchVisionResponse
}

func newVisionStream(ctx context.Context) *visionStream {
	return &visionStream{ctx: ctx, sent: make(chan *platformv1.WatchVisionResponse, 16)}
}

func newBlockingVisionStream(ctx context.Context) *visionStream {
	return &visionStream{ctx: ctx, sent: make(chan *platformv1.WatchVisionResponse, 16), blocked: make(chan struct{}, 1), releaseSend: make(chan struct{})}
}

func (s *visionStream) Send(response *platformv1.WatchVisionResponse) error {
	copy := proto.Clone(response).(*platformv1.WatchVisionResponse)
	s.rawMu.Lock()
	s.raw = append(s.raw, response)
	s.rawMu.Unlock()
	if s.blocked != nil && response.GetRevision() > 1 {
		select {
		case s.blocked <- struct{}{}:
		default:
		}
		select {
		case <-s.releaseSend:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	select {
	case s.sent <- copy:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *visionStream) next(t *testing.T) *platformv1.WatchVisionResponse {
	t.Helper()
	select {
	case snapshot := <-s.sent:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("vision stream did not send snapshot")
		return nil
	}
}

func (s *visionStream) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-s.blocked:
	case <-time.After(time.Second):
		t.Fatal("vision stream did not block")
	}
}

func (s *visionStream) release() { close(s.releaseSend) }

func (s *visionStream) rawSnapshots() []*platformv1.WatchVisionResponse {
	s.rawMu.Lock()
	defer s.rawMu.Unlock()
	return append([]*platformv1.WatchVisionResponse(nil), s.raw...)
}

func (s *visionStream) SetHeader(metadata.MD) error  { return nil }
func (s *visionStream) SendHeader(metadata.MD) error { return nil }
func (s *visionStream) SetTrailer(metadata.MD)       {}
func (s *visionStream) Context() context.Context     { return s.ctx }
func (s *visionStream) SendMsg(any) error            { return nil }
func (s *visionStream) RecvMsg(any) error            { return nil }

type audioStream struct {
	ctx   context.Context
	sent  chan *platformv1.WatchAudioResponse
	rawMu sync.Mutex
	raw   []*platformv1.WatchAudioResponse
}

func newAudioStream(ctx context.Context) *audioStream {
	return &audioStream{ctx: ctx, sent: make(chan *platformv1.WatchAudioResponse, 16)}
}

func (s *audioStream) Send(response *platformv1.WatchAudioResponse) error {
	copy := proto.Clone(response).(*platformv1.WatchAudioResponse)
	s.rawMu.Lock()
	s.raw = append(s.raw, response)
	s.rawMu.Unlock()
	select {
	case s.sent <- copy:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *audioStream) rawSnapshots() []*platformv1.WatchAudioResponse {
	s.rawMu.Lock()
	defer s.rawMu.Unlock()
	return append([]*platformv1.WatchAudioResponse(nil), s.raw...)
}

func (s *audioStream) next(t *testing.T) *platformv1.WatchAudioResponse {
	t.Helper()
	select {
	case snapshot := <-s.sent:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("audio stream did not send snapshot")
		return nil
	}
}

func (s *audioStream) SetHeader(metadata.MD) error  { return nil }
func (s *audioStream) SendHeader(metadata.MD) error { return nil }
func (s *audioStream) SetTrailer(metadata.MD)       {}
func (s *audioStream) Context() context.Context     { return s.ctx }
func (s *audioStream) SendMsg(any) error            { return nil }
func (s *audioStream) RecvMsg(any) error            { return nil }

func waitStream(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("watch stream did not stop")
		return nil
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for index := 0; index <= len(haystack)-len(needle); index++ {
		if reflect.DeepEqual(haystack[index:index+len(needle)], needle) {
			return true
		}
	}
	return false
}

var (
	_ grpc.ServerStreamingServer[platformv1.WatchVisionResponse] = (*visionStream)(nil)
	_ grpc.ServerStreamingServer[platformv1.WatchAudioResponse]  = (*audioStream)(nil)
)
