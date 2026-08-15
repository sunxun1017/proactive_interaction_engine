package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	capabilityregistry "proactive-interaction-engine/adapters/capability/registry"
	identityingress "proactive-interaction-engine/adapters/identity/ingress"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDesktopIdentityFakeProviderEndToEnd(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	registry, err := capabilityregistry.New(clock, time.Minute)
	if err != nil {
		t.Fatalf("registry.New() error = %v", err)
	}
	permissions, err := privacy.New(ctx, &identityE2EPrivacyRepository{}, clock)
	if err != nil {
		t.Fatalf("privacy.New() error = %v", err)
	}
	for _, permission := range []privacy.Permission{
		privacy.CameraCapture, privacy.MicrophoneCapture, privacy.FaceDetection,
		privacy.FaceIdentification, privacy.FaceLiveness, privacy.SpeakerIdentification,
		privacy.SpeakerVerification,
	} {
		if _, err := permissions.Change(ctx, privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("enable %s: %v", permission, err)
		}
	}
	catalog, err := biometric.New(ctx, &identityE2EBiometricRepository{snapshot: identityE2EActiveCatalog(now)}, clock)
	if err != nil {
		t.Fatalf("biometric.New() error = %v", err)
	}

	runtime, err := newDesktopIdentityRuntime(desktopIdentityRuntimeConfig{
		Policy: identity.Policy{
			Version: "identity-policy.e2e", FaceIdentificationThreshold: .8,
			SpeakerIdentificationThreshold: .8, SpeakerVerificationThreshold: .9,
			MaxEvidenceAge: 3 * time.Second, MaxEvidenceSkew: time.Second, RequireFaceLiveness: true,
		},
		Identification: identity.IdentificationCoordinatorConfig{WindowDuration: time.Second, MaxOpenWindows: 4},
		Verification:   identity.SpeakerVerificationCoordinatorConfig{ChallengeDuration: time.Second, MaxOpenChallenges: 2},
	}, permissions, catalog, clock)
	if err != nil {
		t.Fatalf("newDesktopIdentityRuntime() error = %v", err)
	}
	scenario := identityE2EScenario()
	ingress, err := identityingress.NewServer(
		identityingress.Config{MaxTrackedSources: 5}, registry, permissions,
		runtime.identification, runtime.verification, clock, scenario,
	)
	if err != nil {
		t.Fatalf("identity ingress NewServer() error = %v", err)
	}
	registryClient, identityClient := startDesktopIdentityE2ETransport(t, registry, ingress)
	providers := make(map[readiness.CapabilityKind]identityE2EProvider, 5)
	for _, provider := range identityE2EProviders() {
		providers[provider.capability] = registerDesktopIdentityE2EProvider(t, registryClient, provider)
	}

	window, err := runtime.OpenIdentificationWindow("identity-window-e2e")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	challenge, err := runtime.IssueSpeakerVerification("verification-challenge-e2e", "verification-window-e2e", "profile-a")
	if err != nil {
		t.Fatalf("IssueSpeakerVerification() error = %v", err)
	}
	metadata := func(capability readiness.CapabilityKind, fragmentID, windowID string, sequence uint64) *platformv1.IdentityEvidenceMetadata {
		provider := providers[capability]
		return identityE2EMetadata(clock.Now(), provider, fragmentID, windowID, sequence)
	}

	receipts := make([]*platformv1.IdentityEvidenceReceipt, 0, 5)
	detection, err := identityClient.PublishFaceDetectionEvidence(ctx, &platformv1.PublishFaceDetectionEvidenceRequest{
		Metadata: metadata(readiness.FaceDetection, "detection-fragment", window.Token, 1), FacesObserved: 1,
	})
	receipts = appendIdentityE2EReceipt(t, receipts, detection.GetReceipt(), err)
	face, err := identityClient.PublishFaceIdentificationEvidence(ctx, &platformv1.PublishFaceIdentificationEvidenceRequest{
		Metadata: metadata(readiness.FaceIdentification, "face-fragment", window.Token, 1),
		Candidates: []*platformv1.FaceIdentificationCandidate{{
			CandidateId: "face-candidate", ProfileRef: "profile-a", Score: .95, ModelVersion: "face.model.v1",
		}},
	})
	receipts = appendIdentityE2EReceipt(t, receipts, face.GetReceipt(), err)
	liveness, err := identityClient.PublishFaceLivenessEvidence(ctx, &platformv1.PublishFaceLivenessEvidenceRequest{
		Metadata: metadata(readiness.FaceLiveness, "liveness-fragment", window.Token, 1),
		State:    platformv1.FaceLivenessState_FACE_LIVENESS_STATE_PASSED,
	})
	receipts = appendIdentityE2EReceipt(t, receipts, liveness.GetReceipt(), err)
	speaker, err := identityClient.PublishSpeakerIdentificationEvidence(ctx, &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: metadata(readiness.SpeakerIdentification, "speaker-fragment", window.Token, 1),
		Candidates: []*platformv1.SpeakerIdentificationCandidate{{
			CandidateId: "speaker-candidate", ProfileRef: "profile-a", Score: .96, ModelVersion: "speaker.model.v1",
		}},
	})
	receipts = appendIdentityE2EReceipt(t, receipts, speaker.GetReceipt(), err)
	verification, err := identityClient.PublishSpeakerVerificationEvidence(ctx, &platformv1.PublishSpeakerVerificationEvidenceRequest{
		Metadata:                metadata(readiness.SpeakerVerification, "verification-fragment", challenge.EvidenceWindowID, 1),
		VerificationChallengeId: challenge.ID, CandidateId: "verification-candidate",
		Score: .97, ModelVersion: "verification.model.v1",
	})
	receipts = appendIdentityE2EReceipt(t, receipts, verification.GetReceipt(), err)
	for _, receipt := range receipts {
		assertIdentityE2EReceipt(t, receipt,
			platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_ACCEPTED,
			platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_NONE,
		)
		wire, err := proto.Marshal(receipt)
		if err != nil {
			t.Fatalf("marshal receipt: %v", err)
		}
		for _, sensitive := range []string{"profile-a", "candidate", ".model."} {
			if strings.Contains(string(wire), sensitive) {
				t.Fatalf("receipt %q disclosed %q", receipt.GetFragmentId(), sensitive)
			}
		}
	}

	early, err := runtime.AdvanceIdentification(ctx, window.Wakeup())
	if err != nil || early.Resolved {
		t.Fatalf("AdvanceIdentification(before deadline) = %#v, %v", early, err)
	}
	if _, ok := runtime.LatestResolution(); ok {
		t.Fatal("identity resolution appeared before the application deadline")
	}
	clock.Advance(time.Second)
	resolved, err := runtime.AdvanceIdentification(ctx, window.Wakeup())
	if err != nil {
		t.Fatalf("AdvanceIdentification(deadline) error = %v", err)
	}
	if !resolved.Resolved || resolved.Resolution.Assurance != identity.Recognized ||
		resolved.Resolution.ProfileRef != "profile-a" || resolved.Resolution.Reason != identity.ReasonModalitiesMatched {
		t.Fatalf("resolved identity = %#v", resolved)
	}
	baseline, ok := runtime.LatestResolution()
	if !ok || baseline != resolved.Resolution {
		t.Fatalf("LatestResolution() = %#v, %t, want %#v", baseline, ok, resolved.Resolution)
	}

	wrongLease := identityE2EMetadata(clock.Now(), providers[readiness.FaceDetection], "wrong-lease", "unused-window", 2)
	wrongLease.ProviderLeaseId = "missing-lease"
	rejectedLease, err := identityClient.PublishFaceDetectionEvidence(ctx, &platformv1.PublishFaceDetectionEvidenceRequest{Metadata: wrongLease})
	if err != nil {
		t.Fatalf("PublishFaceDetectionEvidence(wrong lease) error = %v", err)
	}
	assertIdentityE2EReceipt(t, rejectedLease.GetReceipt(),
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_REJECTED,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_LEASE_REJECTED,
	)

	permissionWindow, err := runtime.OpenIdentificationWindow("permission-window")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow(permission) error = %v", err)
	}
	if _, err := permissions.Change(ctx, privacy.ChangePermission{Permission: privacy.SpeakerIdentification, Enabled: false}); err != nil {
		t.Fatalf("disable speaker identification: %v", err)
	}
	policyRejected, err := identityClient.PublishSpeakerIdentificationEvidence(ctx, &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: metadata(readiness.SpeakerIdentification, "permission-fragment", permissionWindow.Token, 2),
	})
	if err != nil {
		t.Fatalf("PublishSpeakerIdentificationEvidence(permission) error = %v", err)
	}
	assertIdentityE2EReceipt(t, policyRejected.GetReceipt(),
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_REJECTED,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_POLICY_REJECTED,
	)
	if _, err := permissions.Change(ctx, privacy.ChangePermission{Permission: privacy.SpeakerIdentification, Enabled: true}); err != nil {
		t.Fatalf("restore speaker identification: %v", err)
	}

	unknownWindow, err := identityClient.PublishSpeakerIdentificationEvidence(ctx, &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: metadata(readiness.SpeakerIdentification, "unknown-window-fragment", "unknown-window", 2),
	})
	if err != nil {
		t.Fatalf("PublishSpeakerIdentificationEvidence(unknown window) error = %v", err)
	}
	assertIdentityE2EReceipt(t, unknownWindow.GetReceipt(),
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_STALE,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_STALE,
	)

	sequenceWindow, err := runtime.OpenIdentificationWindow("sequence-window")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow(sequence) error = %v", err)
	}
	acceptedSequence, err := identityClient.PublishSpeakerIdentificationEvidence(ctx, &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: metadata(readiness.SpeakerIdentification, "sequence-fragment", sequenceWindow.Token, 2),
	})
	if err != nil {
		t.Fatalf("PublishSpeakerIdentificationEvidence(sequence) error = %v", err)
	}
	assertIdentityE2EReceipt(t, acceptedSequence.GetReceipt(),
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_ACCEPTED,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_NONE,
	)
	collision, err := identityClient.PublishSpeakerIdentificationEvidence(ctx, &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: metadata(readiness.SpeakerIdentification, "sequence-collision", sequenceWindow.Token, 2),
	})
	if err != nil {
		t.Fatalf("PublishSpeakerIdentificationEvidence(sequence collision) error = %v", err)
	}
	assertIdentityE2EReceipt(t, collision.GetReceipt(),
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_STALE,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_STALE,
	)
	if latest, ok := runtime.LatestResolution(); !ok || latest != baseline {
		t.Fatalf("negative ingress changed latest resolution to %#v, %t; want %#v", latest, ok, baseline)
	}
}

type identityE2EProvider struct {
	capability readiness.CapabilityKind
	providerID string
	instanceID string
	leaseID    string
	device     platformv1.ProviderDeviceClass
	wire       platformv1.ServiceCapabilityKind
}

func identityE2EProviders() []identityE2EProvider {
	return []identityE2EProvider{
		{readiness.FaceDetection, "face-detection", "face-detection-instance", "", platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION},
		{readiness.FaceIdentification, "face-identification", "face-identification-instance", "", platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION},
		{readiness.FaceLiveness, "face-liveness", "face-liveness-instance", "", platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_LIVENESS},
		{readiness.SpeakerIdentification, "speaker-identification", "speaker-identification-instance", "", platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION},
		{readiness.SpeakerVerification, "speaker-verification", "speaker-verification-instance", "", platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE, platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION},
	}
}

func identityE2EScenario() readiness.ScenarioRequirements {
	scenario := readiness.ScenarioRequirements{ID: "desktop-identity-e2e", MinimumIdentityAssurance: readiness.IdentityAssuranceRecognized}
	for _, provider := range identityE2EProviders() {
		scenario.Required = append(scenario.Required, readiness.CapabilityRequirement{
			Kind: provider.capability, ProviderID: provider.providerID,
			Compatibility: readiness.ProviderCompatibility{
				ProtocolVersion: "v1", AllowedPrivacyClasses: []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal},
				MaximumLatency: time.Second, AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
				AllowedDeviceClasses: []readiness.ProviderDeviceClass{identityE2EDevice(provider.capability)},
			},
		})
	}
	return scenario
}

func identityE2EDevice(capability readiness.CapabilityKind) readiness.ProviderDeviceClass {
	switch capability {
	case readiness.FaceDetection, readiness.FaceIdentification, readiness.FaceLiveness:
		return readiness.ProviderDeviceCamera
	default:
		return readiness.ProviderDeviceMicrophone
	}
}

func registerDesktopIdentityE2EProvider(t *testing.T, client platformv1.CapabilityProviderRegistryServiceClient, provider identityE2EProvider) identityE2EProvider {
	t.Helper()
	response, err := client.RegisterCapabilityProvider(context.Background(), &platformv1.RegisterCapabilityProviderRequest{
		ProviderId: provider.providerID, InstanceId: provider.instanceID, ProtocolVersion: "v1", ImplementationVersion: "fake-identity.e2e",
		Capabilities: []platformv1.ServiceCapabilityKind{provider.wire}, Health: platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_HEALTHY,
		HealthReason: platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE,
		OperationalProfile: &platformv1.ProviderOperationalProfile{
			PrivacyClass: platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL, MaximumLatency: durationpb.New(100 * time.Millisecond),
			CancellationSemantics: platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
			DeviceRequirements:    []platformv1.ProviderDeviceClass{provider.device},
		},
	})
	if err != nil {
		t.Fatalf("register %s: %v", provider.capability, err)
	}
	provider.leaseID = response.GetLeaseId()
	return provider
}

func identityE2EMetadata(at time.Time, provider identityE2EProvider, fragmentID, windowID string, sequence uint64) *platformv1.IdentityEvidenceMetadata {
	return &platformv1.IdentityEvidenceMetadata{
		FragmentId: fragmentID, ProviderLeaseId: provider.leaseID, SourceInstanceId: provider.instanceID, SourceSeq: sequence,
		OccurredAt: timestamppb.New(at), Ttl: durationpb.New(time.Second), TraceId: "trace-" + fragmentID, EvidenceWindowId: windowID,
	}
}

func appendIdentityE2EReceipt(t *testing.T, receipts []*platformv1.IdentityEvidenceReceipt, receipt *platformv1.IdentityEvidenceReceipt, err error) []*platformv1.IdentityEvidenceReceipt {
	t.Helper()
	if err != nil {
		t.Fatalf("publish identity evidence: %v", err)
	}
	return append(receipts, receipt)
}

func assertIdentityE2EReceipt(t *testing.T, receipt *platformv1.IdentityEvidenceReceipt, status platformv1.IdentityEvidenceReceiptStatus, reason platformv1.IdentityEvidenceReceiptReason) {
	t.Helper()
	if receipt == nil || receipt.GetStatus() != status || receipt.GetReason() != reason {
		t.Fatalf("identity receipt = %#v, want status %s reason %s", receipt, status, reason)
	}
}

func startDesktopIdentityE2ETransport(t *testing.T, registry *capabilityregistry.Registry, ingress *identityingress.Server) (platformv1.CapabilityProviderRegistryServiceClient, platformv1.IdentityEvidenceIngressServiceClient) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	platformv1.RegisterCapabilityProviderRegistryServiceServer(server, registry)
	platformv1.RegisterIdentityEvidenceIngressServiceServer(server, ingress)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	connection, err := grpc.NewClient(
		"passthrough:///desktop-identity-e2e",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Errorf("close identity client: %v", err)
		}
		server.Stop()
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close identity listener: %v", err)
		}
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
				t.Errorf("serve identity transport: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("identity transport did not stop")
		}
	})
	return platformv1.NewCapabilityProviderRegistryServiceClient(connection), platformv1.NewIdentityEvidenceIngressServiceClient(connection)
}

type identityE2EPrivacyRepository struct {
	mu       sync.Mutex
	snapshot privacy.Snapshot
}

func (r *identityE2EPrivacyRepository) Load(context.Context) (privacy.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := r.snapshot
	copy.Grants = append([]privacy.Grant(nil), copy.Grants...)
	return copy, nil
}

func (r *identityE2EPrivacyRepository) Save(_ context.Context, expected uint64, snapshot privacy.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapshot.Revision != expected {
		return errors.New("privacy revision conflict")
	}
	r.snapshot = snapshot
	r.snapshot.Grants = append([]privacy.Grant(nil), snapshot.Grants...)
	return nil
}

type identityE2EBiometricRepository struct {
	mu       sync.Mutex
	snapshot biometric.Snapshot
}

func (r *identityE2EBiometricRepository) Load(context.Context) (biometric.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneIdentityE2EBiometricSnapshot(r.snapshot), nil
}

func (r *identityE2EBiometricRepository) Save(_ context.Context, expected uint64, snapshot biometric.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapshot.Revision != expected {
		return errors.New("biometric revision conflict")
	}
	r.snapshot = cloneIdentityE2EBiometricSnapshot(snapshot)
	return nil
}

func cloneIdentityE2EBiometricSnapshot(snapshot biometric.Snapshot) biometric.Snapshot {
	snapshot.Records = append([]biometric.Record(nil), snapshot.Records...)
	for index := range snapshot.Records {
		if snapshot.Records[index].PendingStore != nil {
			pending := *snapshot.Records[index].PendingStore
			snapshot.Records[index].PendingStore = &pending
		}
		if snapshot.Records[index].PendingDelete != nil {
			pending := *snapshot.Records[index].PendingDelete
			snapshot.Records[index].PendingDelete = &pending
		}
	}
	return snapshot
}

func identityE2EActiveCatalog(at time.Time) biometric.Snapshot {
	return biometric.Snapshot{Revision: 1, Records: []biometric.Record{
		{
			ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
			Consented: true, ConsentVersion: 1, ConsentUpdatedAt: at,
			TemplateRef: "face-template", ModelVersion: "face.model.v1",
			Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: at,
		},
		{
			ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification,
			Consented: true, ConsentVersion: 1, ConsentUpdatedAt: at,
			TemplateRef: "speaker-template", ModelVersion: "speaker.model.v1",
			Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: at,
		},
		{
			ProfileRef: "profile-a", Capability: readiness.SpeakerVerification,
			Consented: true, ConsentVersion: 1, ConsentUpdatedAt: at,
			TemplateRef: "verification-template", ModelVersion: "verification.model.v1",
			Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: at,
		},
	}}
}
