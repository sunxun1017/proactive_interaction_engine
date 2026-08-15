package ingress

import (
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	capabilityregistry "proactive-interaction-engine/adapters/capability/registry"
	scenarioconfig "proactive-interaction-engine/adapters/config/scenario"
	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/episode"
	"proactive-interaction-engine/internal/domain/event"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
	"proactive-interaction-engine/internal/runtime/lifecycle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRegisteredWorkersCompleteAcceptedWelcomeThroughIngress(t *testing.T) {
	startedAt := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(startedAt)
	registry, err := capabilityregistry.New(clock, 2*time.Hour)
	if err != nil {
		t.Fatalf("registry.New() error = %v", err)
	}
	loaded := loadIngressIntegrationScenario(t)
	driver := fakeembodiment.NewDriver(ingressIntegrationCapabilities(), clock)
	audit := &memorystorage.AuditRecorder{}
	core, err := application.New(application.Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome.v1",
		ConfigHash:             loaded.Hash,
		RandomSeed:             1,
	}, driver, audit, clock)
	if err != nil {
		t.Fatalf("application.New() error = %v", err)
	}
	runner, err := lifecycle.New(lifecycle.DefaultConfig(), core, clock)
	if err != nil {
		t.Fatalf("lifecycle.New() error = %v", err)
	}
	stopRunner := startIngressIntegrationRunner(t, runner)

	ingressServer, err := NewServer(registry, runner, core, staticActivation{readiness.Degraded}, clock, loaded.Requirements)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	registryClient, ingressClient, stopTransport := startIngressIntegrationTransport(t, registry, ingressServer)

	presenceLease := registerIngressIntegrationProvider(
		t,
		registryClient,
		"desktop-presence",
		"presence-worker",
		platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE,
	)
	vadLease := registerIngressIntegrationProvider(
		t,
		registryClient,
		"desktop-vad",
		"vad-worker",
		platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY,
	)

	requireAcceptedIntegrationReceipt(t, publishIngressIntegration(t, ingressClient, integrationPresenceRequest(
		presenceLease, "presence-left", 1, startedAt, false,
	)), "presence-left")
	clock.Advance(45 * time.Minute)
	requireAcceptedIntegrationReceipt(t, publishIngressIntegration(t, ingressClient, integrationPresenceRequest(
		presenceLease, "presence-returned", 2, clock.Now(), true,
	)), "presence-returned")
	requireAcceptedIntegrationReceipt(t, publishIngressIntegration(t, ingressClient, integrationSpeechRequest(
		vadLease, "speech-reply", 1, clock.Now(), .2,
	)), "speech-reply")

	if err := stopTransport(); err != nil {
		t.Fatalf("stop gRPC transport: %v", err)
	}
	if err := stopRunner(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled", err)
	}

	auditSnapshot := audit.Snapshot()
	if len(auditSnapshot.Outcomes) != 1 ||
		auditSnapshot.Outcomes[0].Kind != episode.Accepted ||
		auditSnapshot.Outcomes[0].Reason != episode.ReasonUserReplied {
		t.Fatalf("audit outcomes = %#v, want one ACCEPTED/USER_REPLIED", auditSnapshot.Outcomes)
	}
	foundReply := false
	for _, semanticEvent := range auditSnapshot.Events {
		if semanticEvent.Kind != event.UserReplied {
			continue
		}
		if foundReply || semanticEvent.Confidence != 1 || semanticEvent.SourceObservationID != "speech-reply" {
			t.Fatalf("USER_REPLIED events = %#v, want one confidence=1 from speech-reply", auditSnapshot.Events)
		}
		foundReply = true
	}
	if !foundReply {
		t.Fatalf("audit events = %#v, want USER_REPLIED", auditSnapshot.Events)
	}

	commands := driver.Commands()
	wantActions := []behavior.ActionType{behavior.AttendUser, behavior.Acknowledge, behavior.Speak, behavior.ReturnIdle}
	if len(commands) != len(wantActions) {
		t.Fatalf("driver commands = %#v, want four welcome commands", commands)
	}
	gotActions := make([]behavior.ActionType, 0, len(commands))
	for _, command := range commands {
		gotActions = append(gotActions, command.Action.Type)
	}
	if !reflect.DeepEqual(gotActions, wantActions) {
		t.Fatalf("command action types = %#v, want %#v", gotActions, wantActions)
	}
	if wakeup, ok := core.NextWakeup(); ok {
		t.Fatalf("NextWakeup() = %#v, want none after accepted reply", wakeup)
	}
}

func loadIngressIntegrationScenario(t *testing.T) scenarioconfig.LoadedScenario {
	t.Helper()
	file, err := os.Open("../../../configs/scenarios/anonymous-return-welcome.v1.yaml")
	if err != nil {
		t.Fatalf("open integration scenario: %v", err)
	}
	loaded, loadErr := scenarioconfig.Load(file)
	closeErr := file.Close()
	if loadErr != nil {
		t.Fatalf("load integration scenario: %v", loadErr)
	}
	if closeErr != nil {
		t.Fatalf("close integration scenario: %v", closeErr)
	}
	return loaded
}

func startIngressIntegrationRunner(t *testing.T, runner *lifecycle.Runner) func() error {
	t.Helper()
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()

	var (
		stopOnce sync.Once
		runErr   error
	)
	stopAndJoin := func() error {
		stopOnce.Do(func() {
			cancelRun()
			select {
			case runErr = <-runDone:
			case <-time.After(2 * time.Second):
				runErr = errors.New("runner did not stop")
			}
		})
		return runErr
	}
	t.Cleanup(func() {
		if err := stopAndJoin(); !errors.Is(err, context.Canceled) {
			t.Errorf("Runner.Run() cleanup error = %v, want context.Canceled", err)
		}
	})
	return stopAndJoin
}

func startIngressIntegrationTransport(
	t *testing.T,
	registry *capabilityregistry.Registry,
	ingressServer *Server,
) (
	platformv1.CapabilityProviderRegistryServiceClient,
	platformv1.ObservationIngressServiceClient,
	func() error,
) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	platformv1.RegisterCapabilityProviderRegistryServiceServer(server, registry)
	platformv1.RegisterObservationIngressServiceServer(server, ingressServer)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	var (
		connection *grpc.ClientConn
		stopOnce   sync.Once
		stopErr    error
	)
	stopAndJoin := func() error {
		stopOnce.Do(func() {
			if connection != nil {
				stopErr = errors.Join(stopErr, connection.Close())
			}
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
				stopErr = errors.Join(stopErr, errors.New("gRPC server did not stop"))
			}
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := stopAndJoin(); err != nil {
			t.Errorf("gRPC transport cleanup error = %v", err)
		}
	})

	var err error
	connection, err = grpc.NewClient(
		"passthrough:///proactive-platform",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	return platformv1.NewCapabilityProviderRegistryServiceClient(connection),
		platformv1.NewObservationIngressServiceClient(connection),
		stopAndJoin
}

func registerIngressIntegrationProvider(
	t *testing.T,
	client platformv1.CapabilityProviderRegistryServiceClient,
	providerID string,
	instanceID string,
	capability platformv1.ServiceCapabilityKind,
) string {
	t.Helper()
	device := platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_CAMERA
	if capability == platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY {
		device = platformv1.ProviderDeviceClass_PROVIDER_DEVICE_CLASS_MICROPHONE
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := client.RegisterCapabilityProvider(ctx, &platformv1.RegisterCapabilityProviderRequest{
		ProviderId:            providerID,
		InstanceId:            instanceID,
		ProtocolVersion:       capabilityregistry.SupportedProtocolVersion,
		ImplementationVersion: "integration-v1",
		Capabilities:          []platformv1.ServiceCapabilityKind{capability},
		Health:                platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_HEALTHY,
		HealthReason:          platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE,
		OperationalProfile: &platformv1.ProviderOperationalProfile{
			PrivacyClass:          platformv1.ProviderPrivacyClass_PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
			MaximumLatency:        durationpb.New(time.Second),
			CancellationSemantics: platformv1.ProviderCancellationSemantics_PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
			DeviceRequirements:    []platformv1.ProviderDeviceClass{device},
		},
	})
	if err != nil {
		t.Fatalf("RegisterCapabilityProvider(%s) error = %v", providerID, err)
	}
	if response.GetLeaseId() == "" {
		t.Fatalf("RegisterCapabilityProvider(%s) returned empty lease", providerID)
	}
	return response.GetLeaseId()
}

func publishIngressIntegration(
	t *testing.T,
	client platformv1.ObservationIngressServiceClient,
	request *platformv1.PublishRequest,
) *platformv1.PublishResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := client.Publish(ctx, request)
	if err != nil {
		t.Fatalf("Publish(%s) error = %v", request.GetObservation().GetId(), err)
	}
	return response
}

func requireAcceptedIntegrationReceipt(t *testing.T, response *platformv1.PublishResponse, observationID string) {
	t.Helper()
	if response.GetReceipt().GetObservationId() != observationID ||
		response.GetReceipt().GetStatus() != platformv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED ||
		response.GetReceipt().GetReasonCode() != "" {
		t.Fatalf("Publish(%s) response = %#v, want ACCEPTED receipt", observationID, response)
	}
}

func integrationPresenceRequest(leaseID, observationID string, sequence uint64, occurredAt time.Time, present bool) *platformv1.PublishRequest {
	return &platformv1.PublishRequest{
		ProviderLeaseId: leaseID,
		Observation: &platformv1.ObservationEnvelope{
			Id:         observationID,
			SourceId:   "presence-worker",
			SourceSeq:  sequence,
			OccurredAt: timestamppb.New(occurredAt),
			Ttl:        durationpb.New(time.Minute),
			SubjectId:  "user-1",
			Confidence: .9,
			TraceId:    "trace-" + observationID,
			Payload: &platformv1.ObservationEnvelope_PersonPresence{
				PersonPresence: &platformv1.PersonPresence{Present: present},
			},
		},
	}
}

func integrationSpeechRequest(leaseID, observationID string, sequence uint64, occurredAt time.Time, confidence float32) *platformv1.PublishRequest {
	return &platformv1.PublishRequest{
		ProviderLeaseId: leaseID,
		Observation: &platformv1.ObservationEnvelope{
			Id:         observationID,
			SourceId:   "vad-worker",
			SourceSeq:  sequence,
			OccurredAt: timestamppb.New(occurredAt),
			Ttl:        durationpb.New(time.Minute),
			SubjectId:  "user-1",
			Confidence: confidence,
			TraceId:    "trace-" + observationID,
			Payload: &platformv1.ObservationEnvelope_SpeechActivity{
				SpeechActivity: &platformv1.SpeechActivity{Active: true, AddressingAgent: false},
			},
		},
	}
}

func ingressIntegrationCapabilities() behavior.Capabilities {
	return behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}
}
