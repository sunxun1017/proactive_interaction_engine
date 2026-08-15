package ingress

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	_ ProviderLeaseReader                             = (*identityLeaseReader)(nil)
	_ PermissionReader                                = (*identityPermissionReader)(nil)
	_ IdentificationEvidenceSubmitter                 = (*identitySubmitter)(nil)
	_ SpeakerVerificationEvidenceSubmitter            = (*identitySubmitter)(nil)
	_ platformv1.IdentityEvidenceIngressServiceServer = (*Server)(nil)
)

func TestIdentityIngressMapsFiveRPCsToIndependentApplicationFragments(t *testing.T) {
	now := mapperNow()
	submitter := &identitySubmitter{}
	server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), submitter, 8, identityScenario())

	detection := validFaceDetectionRequest(now)
	requireIdentityReceipt(t, publishFaceDetection(t, server, detection), acceptedStatus, noneReason)
	wantDetection := identity.FaceDetectionFragment{WindowToken: "window-1", FragmentID: "fragment-1", OccurredAt: now.Add(-time.Second), FacesObserved: 0}
	if !reflect.DeepEqual(submitter.detections, []identity.FaceDetectionFragment{wantDetection}) {
		t.Fatalf("face detection submissions = %#v, want %#v", submitter.detections, wantDetection)
	}

	face := validFaceIdentificationRequest(now)
	face.Metadata.FragmentId, face.Metadata.ProviderLeaseId, face.Metadata.SourceInstanceId = "fragment-face", "lease-face-id", "face-id-instance"
	requireIdentityReceipt(t, publishFaceIdentification(t, server, face), acceptedStatus, noneReason)
	wantFace := identity.FaceIdentificationFragment{
		WindowToken: "window-1", FragmentID: "fragment-face", OccurredAt: now.Add(-time.Second),
		Candidates: []identity.FaceIdentificationCandidate{{ID: "face-1", ProfileRef: "profile-a", Score: .91, ModelVersion: "face.v1"}},
	}
	if !reflect.DeepEqual(submitter.faces, []identity.FaceIdentificationFragment{wantFace}) {
		t.Fatalf("face identification submissions = %#v, want %#v", submitter.faces, wantFace)
	}

	liveness := validFaceLivenessRequest(now)
	requireIdentityReceipt(t, publishFaceLiveness(t, server, liveness), acceptedStatus, noneReason)
	wantLiveness := identity.FaceLivenessFragment{WindowToken: "window-1", FragmentID: "fragment-liveness", OccurredAt: now.Add(-time.Second), State: identity.LivenessPassed}
	if !reflect.DeepEqual(submitter.liveness, []identity.FaceLivenessFragment{wantLiveness}) {
		t.Fatalf("face liveness submissions = %#v, want %#v", submitter.liveness, wantLiveness)
	}

	speaker := validSpeakerIdentificationRequest(now)
	requireIdentityReceipt(t, publishSpeakerIdentification(t, server, speaker), acceptedStatus, noneReason)
	wantSpeaker := identity.SpeakerIdentificationFragment{
		WindowToken: "window-1", FragmentID: "fragment-speaker", OccurredAt: now.Add(-time.Second),
		Candidates: []identity.SpeakerIdentificationCandidate{{ID: "speaker-1", ProfileRef: "profile-a", Score: .82, ModelVersion: "speaker.v1"}},
	}
	if !reflect.DeepEqual(submitter.speakers, []identity.SpeakerIdentificationFragment{wantSpeaker}) {
		t.Fatalf("speaker identification submissions = %#v, want %#v", submitter.speakers, wantSpeaker)
	}

	verification := validSpeakerVerificationRequest(now)
	requireIdentityReceipt(t, publishSpeakerVerification(t, server, verification), acceptedStatus, noneReason)
	wantVerification := identity.SpeakerVerificationFragment{
		ChallengeID: "challenge-1", WindowToken: "window-1", FragmentID: "fragment-verification", OccurredAt: now.Add(-time.Second),
		Candidate: identity.SpeakerVerificationSubmissionCandidate{ID: "verification-1", Score: .94, ModelVersion: "verification.v1"},
	}
	if !reflect.DeepEqual(submitter.verifications, []identity.SpeakerVerificationFragment{wantVerification}) {
		t.Fatalf("speaker verification submissions = %#v, want %#v", submitter.verifications, wantVerification)
	}
}

func TestIdentityIngressEnforcesLeaseSelectionAndCompatibility(t *testing.T) {
	now := mapperNow()
	tests := []struct {
		name       string
		mutate     func(*identityLeaseReader, *platformv1.PublishFaceDetectionEvidenceRequest)
		wantReason platformv1.IdentityEvidenceReceiptReason
	}{
		{name: "lease missing", mutate: func(leases *identityLeaseReader, request *platformv1.PublishFaceDetectionEvidenceRequest) {
			request.Metadata.ProviderLeaseId = "missing"
		}, wantReason: leaseRejectedReason},
		{name: "unhealthy", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) { value.Health = readiness.Unhealthy })
		}, wantReason: leaseRejectedReason},
		{name: "exact expiry", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) { value.LeaseExpiresAt = now })
		}, wantReason: leaseRejectedReason},
		{name: "instance mismatch", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) { value.InstanceID = "other-instance" })
		}, wantReason: leaseRejectedReason},
		{name: "capability undeclared", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) {
				value.Capabilities = []readiness.CapabilityKind{readiness.FaceLiveness}
			})
		}, wantReason: leaseRejectedReason},
		{name: "provider not selected", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) { value.ProviderID = "other-provider" })
		}, wantReason: providerNotSelectedReason},
		{name: "protocol incompatible", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) { value.ProtocolVersion = "v2" })
		}, wantReason: leaseRejectedReason},
		{name: "operational profile incompatible", mutate: func(leases *identityLeaseReader, _ *platformv1.PublishFaceDetectionEvidenceRequest) {
			updateIdentityLease(leases, "lease-detection", func(value *readiness.ProviderSnapshot) {
				value.OperationalProfile.PrivacyClass = readiness.ProviderPrivacyRemoteProcessing
			})
		}, wantReason: leaseRejectedReason},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leases := identityLeases(now)
			request := validFaceDetectionRequest(now)
			test.mutate(leases, request)
			submitter := &identitySubmitter{}
			server := newIdentityTestServer(t, leases, allIdentityPermissions(now), submitter, 8, identityScenario())
			receipt := publishFaceDetection(t, server, request)
			requireIdentityReceipt(t, receipt, rejectedStatus, test.wantReason)
			if submitter.callCount() != 0 {
				t.Fatalf("rejected request submitted %d fragments", submitter.callCount())
			}
		})
	}
}

func TestIdentityIngressAppliesGlobalPermissionWithoutEnrollmentQuery(t *testing.T) {
	now := mapperNow()
	permissions := allIdentityPermissions(now)
	for index := range permissions.snapshot.Grants {
		if permissions.snapshot.Grants[index].Permission == privacy.FaceDetection {
			permissions.snapshot.Grants[index].Enabled = false
			permissions.snapshot.Grants[index].UpdatedAt = time.Time{}
		}
	}
	submitter := &identitySubmitter{}
	server := newIdentityTestServer(t, identityLeases(now), permissions, submitter, 8, identityScenario())
	requireIdentityReceipt(t, publishFaceDetection(t, server, validFaceDetectionRequest(now)), rejectedStatus, policyRejectedReason)
	if permissions.calls != 1 || submitter.callCount() != 0 {
		t.Fatalf("permission calls/submissions = %d/%d, want 1/0", permissions.calls, submitter.callCount())
	}
}

func TestIdentityIngressReceiptDoesNotExposeApplicationIdentityState(t *testing.T) {
	now := mapperNow()
	submitter := &identitySubmitter{errors: []error{
		fault.New(fault.PolicyBlocked, "test", errors.New("profile-a enrollment and resolution details")),
	}}
	server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), submitter, 8, identityScenario())
	response, err := server.PublishFaceDetectionEvidence(context.Background(), validFaceDetectionRequest(now))
	if err != nil || response == nil {
		t.Fatalf("PublishFaceDetectionEvidence() = %#v, %v", response, err)
	}
	requireIdentityReceipt(t, response.Receipt, rejectedStatus, policyRejectedReason)
	if encoded := response.String(); strings.Contains(encoded, "profile-a") || strings.Contains(encoded, "enrollment") || strings.Contains(encoded, "resolution") {
		t.Fatalf("receipt leaked application identity state: %q", encoded)
	}
}

func TestIdentityIngressClassifiesTransportAndApplicationFailures(t *testing.T) {
	now := mapperNow()
	t.Run("malformed transport", func(t *testing.T) {
		server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), &identitySubmitter{}, 8, identityScenario())
		response, err := server.PublishFaceDetectionEvidence(context.Background(), nil)
		requireIdentityCode(t, err, codes.InvalidArgument)
		if response != nil {
			t.Fatalf("malformed response = %#v, want nil", response)
		}
	})
	t.Run("expired transport", func(t *testing.T) {
		server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), &identitySubmitter{}, 8, identityScenario())
		request := validFaceDetectionRequest(now)
		request.Metadata.Ttl.Seconds = 1
		requireIdentityReceipt(t, publishFaceDetection(t, server, request), staleStatus, staleReason)
	})
	for _, test := range []struct {
		name       string
		err        error
		wantStatus platformv1.IdentityEvidenceReceiptStatus
		wantReason platformv1.IdentityEvidenceReceiptReason
		wantCode   codes.Code
	}{
		{name: "stale", err: fault.New(fault.StaleInput, "test", errors.New("closed window")), wantStatus: staleStatus, wantReason: staleReason},
		{name: "invalid", err: fault.New(fault.InvalidInput, "test", errors.New("collision")), wantStatus: rejectedStatus, wantReason: invalidReason},
		{name: "policy", err: fault.New(fault.PolicyBlocked, "test", errors.New("second fragment")), wantStatus: rejectedStatus, wantReason: policyRejectedReason},
		{name: "permission", err: fault.New(fault.PermissionDenied, "test", errors.New("revoked")), wantStatus: rejectedStatus, wantReason: policyRejectedReason},
		{name: "unavailable", err: fault.New(fault.Unavailable, "test", errors.New("closed")), wantCode: codes.Unavailable},
		{name: "deadline", err: fault.New(fault.DeadlineExceeded, "test", errors.New("late")), wantCode: codes.DeadlineExceeded},
		{name: "unknown", err: errors.New("boom"), wantCode: codes.Internal},
	} {
		t.Run(test.name, func(t *testing.T) {
			submitter := &identitySubmitter{errors: []error{test.err}}
			server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), submitter, 8, identityScenario())
			response, err := server.PublishFaceDetectionEvidence(context.Background(), validFaceDetectionRequest(now))
			if test.wantCode != codes.OK {
				requireIdentityCode(t, err, test.wantCode)
				if response != nil {
					t.Fatalf("failure response = %#v, want nil", response)
				}
				return
			}
			if err != nil || response == nil {
				t.Fatalf("PublishFaceDetectionEvidence() = %#v, %v", response, err)
			}
			requireIdentityReceipt(t, response.Receipt, test.wantStatus, test.wantReason)
		})
	}
}

func TestIdentityIngressSequencesPerLeaseAndCapability(t *testing.T) {
	now := mapperNow()
	submitter := &identitySubmitter{statuses: []identity.FragmentReceiptStatus{identity.FragmentAccepted, identity.FragmentDuplicate, identity.FragmentDuplicate}}
	server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), submitter, 8, identityScenario())
	request := validFaceDetectionRequest(now)
	requireIdentityReceipt(t, publishFaceDetection(t, server, request), acceptedStatus, noneReason)
	requireIdentityReceipt(t, publishFaceDetection(t, server, proto.Clone(request).(*platformv1.PublishFaceDetectionEvidenceRequest)), duplicateStatus, duplicateReason)

	changed := proto.Clone(request).(*platformv1.PublishFaceDetectionEvidenceRequest)
	changed.Metadata.FragmentId = "fragment-other"
	requireIdentityReceipt(t, publishFaceDetection(t, server, changed), staleStatus, staleReason)
	higherDuplicate := proto.Clone(request).(*platformv1.PublishFaceDetectionEvidenceRequest)
	higherDuplicate.Metadata.SourceSeq++
	requireIdentityReceipt(t, publishFaceDetection(t, server, higherDuplicate), duplicateStatus, duplicateReason)
	older := proto.Clone(request).(*platformv1.PublishFaceDetectionEvidenceRequest)
	older.Metadata.FragmentId = "fragment-older"
	requireIdentityReceipt(t, publishFaceDetection(t, server, older), staleStatus, staleReason)
	if submitter.callCount() != 3 {
		t.Fatalf("submit count = %d, want 3", submitter.callCount())
	}

	liveness := validFaceLivenessRequest(now)
	liveness.Metadata.ProviderLeaseId = request.Metadata.ProviderLeaseId
	liveness.Metadata.SourceInstanceId = request.Metadata.SourceInstanceId
	leases := server.leases.(*identityLeaseReader)
	lease := leases.snapshots[request.Metadata.ProviderLeaseId]
	lease.Capabilities = append(lease.Capabilities, readiness.FaceLiveness)
	leases.snapshots[request.Metadata.ProviderLeaseId] = lease
	scenario := identityScenario()
	for index := range scenario.Required {
		if scenario.Required[index].Kind == readiness.FaceLiveness {
			scenario.Required[index].ProviderID = lease.ProviderID
		}
	}
	otherServer := newIdentityTestServer(t, leases, allIdentityPermissions(now), &identitySubmitter{}, 8, scenario)
	requireIdentityReceipt(t, publishFaceDetection(t, otherServer, request), acceptedStatus, noneReason)
	requireIdentityReceipt(t, publishFaceLiveness(t, otherServer, liveness), acceptedStatus, noneReason)
}

func TestIdentityIngressDoesNotAdvanceSequenceAfterRejectedSubmission(t *testing.T) {
	now := mapperNow()
	submitter := &identitySubmitter{errors: []error{fault.New(fault.PolicyBlocked, "test", errors.New("reject")), nil}}
	server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), submitter, 8, identityScenario())
	first := validFaceDetectionRequest(now)
	requireIdentityReceipt(t, publishFaceDetection(t, server, first), rejectedStatus, policyRejectedReason)
	second := proto.Clone(first).(*platformv1.PublishFaceDetectionEvidenceRequest)
	second.Metadata.FragmentId = "fragment-retry"
	requireIdentityReceipt(t, publishFaceDetection(t, server, second), acceptedStatus, noneReason)
}

func TestIdentityIngressBoundsAndPrunesSourceStreamsAgainstCurrentLease(t *testing.T) {
	now := mapperNow()
	clock := engineclock.NewFake(now)
	leases := identityLeases(now)
	server, err := NewServer(Config{MaxTrackedSources: 1}, leases, allIdentityPermissions(now), &identitySubmitter{}, &identitySubmitter{}, clock, identityScenario())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	requireIdentityReceipt(t, publishFaceDetection(t, server, validFaceDetectionRequest(now)), acceptedStatus, noneReason)

	original := leases.snapshots["lease-detection"]
	original.LeaseExpiresAt = now.Add(time.Minute)
	leases.snapshots["lease-detection"] = original
	clock.Advance(2 * time.Second)
	response, err := server.PublishFaceLivenessEvidence(context.Background(), validFaceLivenessRequest(clock.Now()))
	requireIdentityCode(t, err, codes.Unavailable)
	if response != nil {
		t.Fatalf("capacity response = %#v, want nil", response)
	}

	original.LeaseExpiresAt = clock.Now()
	leases.snapshots["lease-detection"] = original
	requireIdentityReceipt(t, publishFaceLiveness(t, server, validFaceLivenessRequest(clock.Now())), acceptedStatus, noneReason)
}

func TestIdentityIngressSerializesConcurrentEqualSequences(t *testing.T) {
	now := mapperNow()
	submitter := &identitySubmitter{}
	server := newIdentityTestServer(t, identityLeases(now), allIdentityPermissions(now), submitter, 8, identityScenario())
	start := make(chan struct{})
	receipts := make(chan *platformv1.IdentityEvidenceReceipt, 2)
	for _, fragmentID := range []string{"concurrent-a", "concurrent-b"} {
		fragmentID := fragmentID
		go func() {
			<-start
			request := validFaceDetectionRequest(now)
			request.Metadata.FragmentId = fragmentID
			response, err := server.PublishFaceDetectionEvidence(context.Background(), request)
			if err != nil {
				receipts <- nil
				return
			}
			receipts <- response.Receipt
		}()
	}
	close(start)
	first, second := <-receipts, <-receipts
	accepted, stale := 0, 0
	for _, receipt := range []*platformv1.IdentityEvidenceReceipt{first, second} {
		if receipt == nil {
			t.Fatal("concurrent publish returned error")
		}
		switch receipt.Status {
		case acceptedStatus:
			accepted++
		case staleStatus:
			stale++
		}
	}
	if accepted != 1 || stale != 1 || submitter.callCount() != 1 {
		t.Fatalf("accepted/stale/submitted = %d/%d/%d, want 1/1/1", accepted, stale, submitter.callCount())
	}
}

func TestIdentityIngressConstructorRejectsInvalidDependenciesAndCopiesScenario(t *testing.T) {
	now := mapperNow()
	validConfig := Config{MaxTrackedSources: 1}
	leases, permissions, submitter, clock, scenario := identityLeases(now), allIdentityPermissions(now), &identitySubmitter{}, engineclock.NewFake(now), identityScenario()
	for _, test := range []struct {
		name           string
		config         Config
		leases         ProviderLeaseReader
		permissions    PermissionReader
		identification IdentificationEvidenceSubmitter
		verification   SpeakerVerificationEvidenceSubmitter
		clock          *engineclock.Fake
		scenario       readiness.ScenarioRequirements
	}{
		{name: "capacity", leases: leases, permissions: permissions, identification: submitter, verification: submitter, clock: clock, scenario: scenario},
		{name: "leases", config: validConfig, permissions: permissions, identification: submitter, verification: submitter, clock: clock, scenario: scenario},
		{name: "permissions", config: validConfig, leases: leases, identification: submitter, verification: submitter, clock: clock, scenario: scenario},
		{name: "identification", config: validConfig, leases: leases, permissions: permissions, verification: submitter, clock: clock, scenario: scenario},
		{name: "verification", config: validConfig, leases: leases, permissions: permissions, identification: submitter, clock: clock, scenario: scenario},
		{name: "clock", config: validConfig, leases: leases, permissions: permissions, identification: submitter, verification: submitter, scenario: scenario},
		{name: "scenario", config: validConfig, leases: leases, permissions: permissions, identification: submitter, verification: submitter, clock: clock},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(test.config, test.leases, test.permissions, test.identification, test.verification, test.clock, test.scenario)
			if err == nil || server != nil {
				t.Fatalf("NewServer() = %#v, %v, want nil/error", server, err)
			}
		})
	}

	server := newIdentityTestServer(t, leases, permissions, submitter, 1, scenario)
	scenario.Required[0].ProviderID = "mutated"
	scenario.Required[0].Compatibility.ProtocolVersion = "v2"
	scenario.Required[0].Compatibility.AllowedPrivacyClasses[0] = readiness.ProviderPrivacyRemoteProcessing
	requireIdentityReceipt(t, publishFaceDetection(t, server, validFaceDetectionRequest(now)), acceptedStatus, noneReason)

	if server, err := NewServer(Config{MaxTrackedSources: maxTrackedSources + 1}, leases, permissions, submitter, submitter, clock, identityScenario()); err == nil || server != nil {
		t.Fatalf("NewServer(over maximum) = %#v, %v, want nil/error", server, err)
	}
	if server, err := NewServer(Config{MaxTrackedSources: maxTrackedSources}, leases, permissions, submitter, submitter, clock, identityScenario()); err != nil || server == nil {
		t.Fatalf("NewServer(exact maximum) = %#v, %v, want server", server, err)
	}
}

const (
	acceptedStatus            = platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_ACCEPTED
	duplicateStatus           = platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_DUPLICATE
	staleStatus               = platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_STALE
	rejectedStatus            = platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_REJECTED
	noneReason                = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_NONE
	invalidReason             = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_INVALID
	duplicateReason           = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_DUPLICATE
	staleReason               = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_STALE
	leaseRejectedReason       = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_LEASE_REJECTED
	providerNotSelectedReason = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_PROVIDER_NOT_SELECTED
	policyRejectedReason      = platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_POLICY_REJECTED
)

func newIdentityTestServer(t *testing.T, leases ProviderLeaseReader, permissions PermissionReader, submitter *identitySubmitter, capacity int, scenario readiness.ScenarioRequirements) *Server {
	t.Helper()
	server, err := NewServer(Config{MaxTrackedSources: capacity}, leases, permissions, submitter, submitter, engineclock.NewFake(mapperNow()), scenario)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func identityScenario() readiness.ScenarioRequirements {
	capabilities := []struct {
		kind     readiness.CapabilityKind
		provider string
	}{
		{readiness.FaceDetection, "face-detection"},
		{readiness.FaceIdentification, "face-identification"},
		{readiness.FaceLiveness, "face-liveness"},
		{readiness.SpeakerIdentification, "speaker-identification"},
		{readiness.SpeakerVerification, "speaker-verification"},
	}
	scenario := readiness.ScenarioRequirements{ID: "identity-ingress", MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous}
	for _, capability := range capabilities {
		scenario.Required = append(scenario.Required, readiness.CapabilityRequirement{Kind: capability.kind, ProviderID: capability.provider, Compatibility: identityCompatibility()})
	}
	return scenario
}

func identityCompatibility() readiness.ProviderCompatibility {
	return readiness.ProviderCompatibility{
		ProtocolVersion: "v1", AllowedPrivacyClasses: []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal}, MaximumLatency: time.Second,
		AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
		AllowedDeviceClasses:         []readiness.ProviderDeviceClass{readiness.ProviderDeviceCamera, readiness.ProviderDeviceMicrophone},
	}
}

func identityLeases(now time.Time) *identityLeaseReader {
	return &identityLeaseReader{snapshots: map[string]readiness.ProviderSnapshot{
		"lease-detection":    identityProvider("face-detection", "face-instance", now, readiness.FaceDetection),
		"lease-face-id":      identityProvider("face-identification", "face-id-instance", now, readiness.FaceIdentification),
		"lease-liveness":     identityProvider("face-liveness", "liveness-instance", now, readiness.FaceLiveness),
		"lease-speaker":      identityProvider("speaker-identification", "speaker-instance", now, readiness.SpeakerIdentification),
		"lease-verification": identityProvider("speaker-verification", "verification-instance", now, readiness.SpeakerVerification),
	}}
}

func identityProvider(providerID, instanceID string, now time.Time, capability readiness.CapabilityKind) readiness.ProviderSnapshot {
	device := readiness.ProviderDeviceCamera
	if capability == readiness.SpeakerIdentification || capability == readiness.SpeakerVerification {
		device = readiness.ProviderDeviceMicrophone
	}
	return readiness.ProviderSnapshot{
		ProviderID: providerID, InstanceID: instanceID, ProtocolVersion: "v1", ImplementationVersion: "test.v1",
		Capabilities: []readiness.CapabilityKind{capability}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(30 * time.Second),
		OperationalProfile: readiness.ProviderOperationalProfile{PrivacyClass: readiness.ProviderPrivacyDeviceLocal, MaximumLatency: 100 * time.Millisecond, CancellationSemantics: readiness.ProviderCancellationCooperative, DeviceRequirements: []readiness.ProviderDeviceClass{device}},
	}
}

func allIdentityPermissions(now time.Time) *identityPermissionReader {
	snapshot := privacy.Snapshot{Revision: 1}
	for _, permission := range privacy.AllPermissions() {
		snapshot.Grants = append(snapshot.Grants, privacy.Grant{Permission: permission, Enabled: true, UpdatedAt: now})
	}
	return &identityPermissionReader{snapshot: snapshot}
}

func validFaceDetectionRequest(now time.Time) *platformv1.PublishFaceDetectionEvidenceRequest {
	metadata := validMetadata(now)
	metadata.ProviderLeaseId, metadata.SourceInstanceId = "lease-detection", "face-instance"
	return &platformv1.PublishFaceDetectionEvidenceRequest{Metadata: metadata}
}

func validFaceIdentificationRequest(now time.Time) *platformv1.PublishFaceIdentificationEvidenceRequest {
	return &platformv1.PublishFaceIdentificationEvidenceRequest{Metadata: validMetadata(now), Candidates: []*platformv1.FaceIdentificationCandidate{{CandidateId: "face-1", ProfileRef: "profile-a", Score: .91, ModelVersion: "face.v1"}}}
}

func validFaceLivenessRequest(now time.Time) *platformv1.PublishFaceLivenessEvidenceRequest {
	metadata := validMetadata(now)
	metadata.FragmentId, metadata.ProviderLeaseId, metadata.SourceInstanceId = "fragment-liveness", "lease-liveness", "liveness-instance"
	return &platformv1.PublishFaceLivenessEvidenceRequest{Metadata: metadata, State: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_PASSED}
}

func validSpeakerIdentificationRequest(now time.Time) *platformv1.PublishSpeakerIdentificationEvidenceRequest {
	metadata := validMetadata(now)
	metadata.FragmentId, metadata.ProviderLeaseId, metadata.SourceInstanceId = "fragment-speaker", "lease-speaker", "speaker-instance"
	return &platformv1.PublishSpeakerIdentificationEvidenceRequest{Metadata: metadata, Candidates: []*platformv1.SpeakerIdentificationCandidate{{CandidateId: "speaker-1", ProfileRef: "profile-a", Score: .82, ModelVersion: "speaker.v1"}}}
}

func validSpeakerVerificationRequest(now time.Time) *platformv1.PublishSpeakerVerificationEvidenceRequest {
	metadata := validMetadata(now)
	metadata.FragmentId, metadata.ProviderLeaseId, metadata.SourceInstanceId = "fragment-verification", "lease-verification", "verification-instance"
	return &platformv1.PublishSpeakerVerificationEvidenceRequest{Metadata: metadata, VerificationChallengeId: "challenge-1", CandidateId: "verification-1", Score: .94, ModelVersion: "verification.v1"}
}

func publishFaceDetection(t *testing.T, server *Server, request *platformv1.PublishFaceDetectionEvidenceRequest) *platformv1.IdentityEvidenceReceipt {
	t.Helper()
	response, err := server.PublishFaceDetectionEvidence(context.Background(), request)
	if err != nil {
		t.Fatalf("PublishFaceDetectionEvidence() error = %v", err)
	}
	return response.Receipt
}
func publishFaceIdentification(t *testing.T, server *Server, request *platformv1.PublishFaceIdentificationEvidenceRequest) *platformv1.IdentityEvidenceReceipt {
	t.Helper()
	response, err := server.PublishFaceIdentificationEvidence(context.Background(), request)
	if err != nil {
		t.Fatalf("PublishFaceIdentificationEvidence() error = %v", err)
	}
	return response.Receipt
}
func publishFaceLiveness(t *testing.T, server *Server, request *platformv1.PublishFaceLivenessEvidenceRequest) *platformv1.IdentityEvidenceReceipt {
	t.Helper()
	response, err := server.PublishFaceLivenessEvidence(context.Background(), request)
	if err != nil {
		t.Fatalf("PublishFaceLivenessEvidence() error = %v", err)
	}
	return response.Receipt
}
func publishSpeakerIdentification(t *testing.T, server *Server, request *platformv1.PublishSpeakerIdentificationEvidenceRequest) *platformv1.IdentityEvidenceReceipt {
	t.Helper()
	response, err := server.PublishSpeakerIdentificationEvidence(context.Background(), request)
	if err != nil {
		t.Fatalf("PublishSpeakerIdentificationEvidence() error = %v", err)
	}
	return response.Receipt
}
func publishSpeakerVerification(t *testing.T, server *Server, request *platformv1.PublishSpeakerVerificationEvidenceRequest) *platformv1.IdentityEvidenceReceipt {
	t.Helper()
	response, err := server.PublishSpeakerVerificationEvidence(context.Background(), request)
	if err != nil {
		t.Fatalf("PublishSpeakerVerificationEvidence() error = %v", err)
	}
	return response.Receipt
}

func requireIdentityReceipt(t *testing.T, receipt *platformv1.IdentityEvidenceReceipt, wantStatus platformv1.IdentityEvidenceReceiptStatus, wantReason platformv1.IdentityEvidenceReceiptReason) {
	t.Helper()
	if receipt == nil || receipt.Status != wantStatus || receipt.Reason != wantReason {
		t.Fatalf("receipt = %#v, want %s/%s", receipt, wantStatus, wantReason)
	}
}

func requireIdentityCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("status = %s for %v, want %s", status.Code(err), err, want)
	}
}

type identityLeaseReader struct {
	snapshots map[string]readiness.ProviderSnapshot
}

func updateIdentityLease(reader *identityLeaseReader, id string, mutate func(*readiness.ProviderSnapshot)) {
	value := reader.snapshots[id]
	mutate(&value)
	reader.snapshots[id] = value
}

func (f *identityLeaseReader) LeaseSnapshot(id string) (readiness.ProviderSnapshot, bool) {
	value, ok := f.snapshots[id]
	return value, ok
}

type identityPermissionReader struct {
	mu       sync.Mutex
	snapshot privacy.Snapshot
	err      error
	calls    int
}

func (f *identityPermissionReader) Current(context.Context) (privacy.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.snapshot, f.err
}

type identitySubmitter struct {
	mu            sync.Mutex
	detections    []identity.FaceDetectionFragment
	faces         []identity.FaceIdentificationFragment
	liveness      []identity.FaceLivenessFragment
	speakers      []identity.SpeakerIdentificationFragment
	verifications []identity.SpeakerVerificationFragment
	statuses      []identity.FragmentReceiptStatus
	errors        []error
}

func (f *identitySubmitter) result(fragmentID string) (identity.FragmentReceipt, error) {
	index := len(f.detections) + len(f.faces) + len(f.liveness) + len(f.speakers) + len(f.verifications) - 1
	var err error
	if index < len(f.errors) {
		err = f.errors[index]
	}
	status := identity.FragmentAccepted
	if index < len(f.statuses) && f.statuses[index] != "" {
		status = f.statuses[index]
	}
	return identity.FragmentReceipt{FragmentID: fragmentID, Status: status}, err
}
func (f *identitySubmitter) SubmitFaceDetectionAt(value identity.FaceDetectionFragment, _ time.Time) (identity.FragmentReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detections = append(f.detections, value)
	return f.result(value.FragmentID)
}
func (f *identitySubmitter) SubmitFaceIdentificationAt(value identity.FaceIdentificationFragment, _ time.Time) (identity.FragmentReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faces = append(f.faces, value)
	return f.result(value.FragmentID)
}
func (f *identitySubmitter) SubmitFaceLivenessAt(value identity.FaceLivenessFragment, _ time.Time) (identity.FragmentReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveness = append(f.liveness, value)
	return f.result(value.FragmentID)
}
func (f *identitySubmitter) SubmitSpeakerIdentificationAt(value identity.SpeakerIdentificationFragment, _ time.Time) (identity.FragmentReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.speakers = append(f.speakers, value)
	return f.result(value.FragmentID)
}
func (f *identitySubmitter) SubmitSpeakerVerificationAt(value identity.SpeakerVerificationFragment, _ time.Time) (identity.FragmentReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifications = append(f.verifications, value)
	return f.result(value.FragmentID)
}
func (f *identitySubmitter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.detections) + len(f.faces) + len(f.liveness) + len(f.speakers) + len(f.verifications)
}
