package ingress

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	capabilityregistry "proactive-interaction-engine/adapters/capability/registry"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRegisteredIdentityProviderSubmitsToApplicationOwnedWindows(t *testing.T) {
	now := mapperNow()
	clock := engineclock.NewFake(now)
	registry, err := capabilityregistry.New(clock, time.Minute)
	if err != nil {
		t.Fatalf("registry.New() error = %v", err)
	}
	permissions, err := privacy.New(context.Background(), &identityMemoryPermissions{}, clock)
	if err != nil {
		t.Fatalf("privacy.New() error = %v", err)
	}
	for _, permission := range []privacy.Permission{
		privacy.CameraCapture, privacy.MicrophoneCapture, privacy.FaceDetection,
		privacy.FaceIdentification, privacy.FaceLiveness, privacy.SpeakerIdentification,
		privacy.SpeakerVerification,
	} {
		if _, err := permissions.Change(context.Background(), privacy.ChangePermission{Permission: permission, Enabled: true}); err != nil {
			t.Fatalf("enable %s: %v", permission, err)
		}
	}

	identification, err := identity.NewEvidenceCoordinator(identity.IdentificationCoordinatorConfig{WindowDuration: 2 * time.Second, MaxOpenWindows: 1})
	if err != nil {
		t.Fatalf("NewEvidenceCoordinator() error = %v", err)
	}
	identificationWindow, err := identification.OpenIdentificationWindow("identification-window", now)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	verification, err := identity.NewSpeakerVerificationCoordinator(identity.SpeakerVerificationCoordinatorConfig{ChallengeDuration: 2 * time.Second, MaxOpenChallenges: 1})
	if err != nil {
		t.Fatalf("NewSpeakerVerificationCoordinator() error = %v", err)
	}
	challenge, err := verification.IssueChallenge("verification-challenge", "verification-window", "profile-a", now)
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}

	scenario := integratedIdentityScenario()
	ingressServer, err := NewServer(Config{MaxTrackedSources: 5}, registry, permissions, identification, verification, clock, scenario)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	registryClient, identityClient, stop := startIdentityIntegrationTransport(t, registry, ingressServer)
	providers := map[readiness.CapabilityKind]integratedIdentityProvider{
		readiness.FaceDetection:         registerIntegratedIdentityProvider(t, registryClient, "face-detection", "face-detection-instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION, platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA),
		readiness.FaceIdentification:    registerIntegratedIdentityProvider(t, registryClient, "face-identification", "face-identification-instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION, platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA),
		readiness.FaceLiveness:          registerIntegratedIdentityProvider(t, registryClient, "face-liveness", "face-liveness-instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_LIVENESS, platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA),
		readiness.SpeakerIdentification: registerIntegratedIdentityProvider(t, registryClient, "speaker-identification", "speaker-identification-instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION, platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE),
		readiness.SpeakerVerification:   registerIntegratedIdentityProvider(t, registryClient, "speaker-verification", "speaker-verification-instance", platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION, platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE),
	}

	metadata := func(fragmentID, windowID string, capability readiness.CapabilityKind) *platformv1.IdentityEvidenceMetadata {
		provider := providers[capability]
		return &platformv1.IdentityEvidenceMetadata{
			FragmentId: fragmentID, ProviderLeaseId: provider.leaseID, SourceInstanceId: provider.instanceID, SourceSeq: 1,
			OccurredAt: timestamppb.New(now), Ttl: durationpb.New(2 * time.Second), TraceId: "trace-" + fragmentID,
			EvidenceWindowId: windowID,
		}
	}
	receipts := make([]*platformv1.IdentityEvidenceReceipt, 0, 5)
	detection, err := identityClient.PublishFaceDetectionEvidence(context.Background(), &platformv1.PublishFaceDetectionEvidenceRequest{
		Metadata: metadata("detection-fragment", identificationWindow.Token, readiness.FaceDetection), FacesObserved: 0,
	})
	if err != nil {
		t.Fatalf("PublishFaceDetectionEvidence() error = %v", err)
	}
	receipts = append(receipts, detection.Receipt)
	face, err := identityClient.PublishFaceIdentificationEvidence(context.Background(), &platformv1.PublishFaceIdentificationEvidenceRequest{
		Metadata: metadata("face-fragment", identificationWindow.Token, readiness.FaceIdentification),
	})
	if err != nil {
		t.Fatalf("PublishFaceIdentificationEvidence() error = %v", err)
	}
	receipts = append(receipts, face.Receipt)
	liveness, err := identityClient.PublishFaceLivenessEvidence(context.Background(), &platformv1.PublishFaceLivenessEvidenceRequest{
		Metadata: metadata("liveness-fragment", identificationWindow.Token, readiness.FaceLiveness), State: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN,
	})
	if err != nil {
		t.Fatalf("PublishFaceLivenessEvidence() error = %v", err)
	}
	receipts = append(receipts, liveness.Receipt)
	speaker, err := identityClient.PublishSpeakerIdentificationEvidence(context.Background(), &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: metadata("speaker-fragment", identificationWindow.Token, readiness.SpeakerIdentification),
	})
	if err != nil {
		t.Fatalf("PublishSpeakerIdentificationEvidence() error = %v", err)
	}
	receipts = append(receipts, speaker.Receipt)
	verified, err := identityClient.PublishSpeakerVerificationEvidence(context.Background(), &platformv1.PublishSpeakerVerificationEvidenceRequest{
		Metadata: metadata("verification-fragment", challenge.EvidenceWindowID, readiness.SpeakerVerification), VerificationChallengeId: challenge.ID,
		CandidateId: "verification-candidate", Score: .95, ModelVersion: "speaker-verification.v1",
	})
	if err != nil {
		t.Fatalf("PublishSpeakerVerificationEvidence() error = %v", err)
	}
	receipts = append(receipts, verified.Receipt)
	for _, receipt := range receipts {
		requireIdentityReceipt(t, receipt, acceptedStatus, noneReason)
	}

	identificationAdvance, err := identification.AdvanceAt(identificationWindow.Wakeup(), identity.Policy{}, privacy.Snapshot{}, biometric.Snapshot{}, now)
	if err != nil || identificationAdvance.Resolved {
		t.Fatalf("identification AdvanceAt(before deadline) = %#v, %v", identificationAdvance, err)
	}
	verificationAdvance, err := verification.AdvanceAt(challenge.Wakeup(), identity.Policy{}, privacy.Snapshot{}, biometric.Snapshot{}, now)
	if err != nil || verificationAdvance.Resolved {
		t.Fatalf("verification AdvanceAt(before deadline) = %#v, %v", verificationAdvance, err)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop transport: %v", err)
	}
}

func integratedIdentityScenario() readiness.ScenarioRequirements {
	scenario := readiness.ScenarioRequirements{ID: "integrated-identity", MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous}
	for _, selection := range []struct {
		capability readiness.CapabilityKind
		providerID string
	}{
		{readiness.FaceDetection, "face-detection"},
		{readiness.FaceIdentification, "face-identification"},
		{readiness.FaceLiveness, "face-liveness"},
		{readiness.SpeakerIdentification, "speaker-identification"},
		{readiness.SpeakerVerification, "speaker-verification"},
	} {
		scenario.Required = append(scenario.Required, readiness.CapabilityRequirement{
			Kind: selection.capability, ProviderID: selection.providerID, Compatibility: identityCompatibility(),
		})
	}
	return scenario
}

type integratedIdentityProvider struct {
	leaseID    string
	instanceID string
}

func registerIntegratedIdentityProvider(
	t *testing.T,
	client platformv1.CapabilityProviderRegistryServiceClient,
	providerID string,
	instanceID string,
	capability platformv1.ServiceCapabilityKind,
	device platformv1.ProviderDeviceClass,
) integratedIdentityProvider {
	t.Helper()
	response, err := client.RegisterCapabilityProvider(context.Background(), &platformv1.RegisterCapabilityProviderRequest{
		ProviderId: providerID, InstanceId: instanceID, ProtocolVersion: "v1", ImplementationVersion: "identity-test.v1",
		Capabilities: []platformv1.ServiceCapabilityKind{capability},
		OperationalProfile: &platformv1.ProviderOperationalProfile{
			PrivacyClass:          platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
			MaximumLatency:        durationpb.New(100 * time.Millisecond),
			CancellationSemantics: platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
			DeviceRequirements:    []platformv1.ProviderDeviceClass{device},
		},
		Health:       platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_HEALTHY,
		HealthReason: platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE,
	})
	if err != nil {
		t.Fatalf("RegisterCapabilityProvider() error = %v", err)
	}
	return integratedIdentityProvider{leaseID: response.LeaseId, instanceID: instanceID}
}

func startIdentityIntegrationTransport(
	t *testing.T,
	registry *capabilityregistry.Registry,
	ingressServer *Server,
) (platformv1.CapabilityProviderRegistryServiceClient, platformv1.IdentityEvidenceIngressServiceClient, func() error) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	platformv1.RegisterCapabilityProviderRegistryServiceServer(server, registry)
	platformv1.RegisterIdentityEvidenceIngressServiceServer(server, ingressServer)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	connection, err := grpc.NewClient(
		"passthrough:///identity-platform",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			stopErr = errors.Join(stopErr, connection.Close())
			server.Stop()
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				stopErr = errors.Join(stopErr, err)
			}
			select {
			case err := <-serveDone:
				if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
					stopErr = errors.Join(stopErr, err)
				}
			case <-time.After(2 * time.Second):
				stopErr = errors.Join(stopErr, errors.New("identity integration transport did not stop"))
			}
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop identity integration transport: %v", err)
		}
	})
	return platformv1.NewCapabilityProviderRegistryServiceClient(connection), platformv1.NewIdentityEvidenceIngressServiceClient(connection), stop
}

type identityMemoryPermissions struct {
	snapshot privacy.Snapshot
}

func (m *identityMemoryPermissions) Load(context.Context) (privacy.Snapshot, error) {
	return m.snapshot, nil
}
func (m *identityMemoryPermissions) Save(_ context.Context, _ uint64, snapshot privacy.Snapshot) error {
	m.snapshot = snapshot
	return nil
}
