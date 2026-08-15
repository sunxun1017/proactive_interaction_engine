package ingress

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	_ ProviderLeaseReader                        = (*fakeLeaseReader)(nil)
	_ ObservationSubmitter                       = (*fakeSubmitter)(nil)
	_ ReplyWindowReader                          = (*fakeWindowReader)(nil)
	_ ActivationReader                           = staticActivation{}
	_ platformv1.ObservationIngressServiceServer = (*Server)(nil)
)

func TestNewServerValidatesDependenciesAndCopiesScenario(t *testing.T) {
	now := ingressTestNow()
	clock := engineclock.NewFake(now)
	leases := validLeaseReader(now)
	submitter := &fakeSubmitter{}
	windows := &fakeWindowReader{}
	scenario := validScenario()

	tests := []struct {
		name       string
		leases     ProviderLeaseReader
		submitter  ObservationSubmitter
		windows    ReplyWindowReader
		activation ActivationReader
		clock      *engineclock.Fake
		scenario   readiness.ScenarioRequirements
	}{
		{name: "nil leases", submitter: submitter, windows: windows, activation: staticActivation{readiness.Ready}, clock: clock, scenario: scenario},
		{name: "nil submitter", leases: leases, windows: windows, activation: staticActivation{readiness.Ready}, clock: clock, scenario: scenario},
		{name: "nil windows", leases: leases, submitter: submitter, activation: staticActivation{readiness.Ready}, clock: clock, scenario: scenario},
		{name: "nil activation", leases: leases, submitter: submitter, windows: windows, clock: clock, scenario: scenario},
		{name: "nil clock", leases: leases, submitter: submitter, windows: windows, activation: staticActivation{readiness.Ready}, scenario: scenario},
		{name: "invalid scenario", leases: leases, submitter: submitter, windows: windows, activation: staticActivation{readiness.Ready}, clock: clock},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(test.leases, test.submitter, test.windows, test.activation, test.clock, test.scenario)
			if err == nil || server != nil {
				t.Fatalf("NewServer() = %#v, %v, want nil and error", server, err)
			}
		})
	}

	server := newTestServer(t, leases, submitter, windows, clock, scenario)
	scenario.Required[0].ProviderID = "mutated-after-construction"
	receipt := publishReceipt(t, server, validPresenceRequest(now))
	requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED, "")
}

func TestPublishFailsClosedWhileRequiredScenarioCapabilitiesAreBlocked(t *testing.T) {
	now := ingressTestNow()
	submitter := &fakeSubmitter{}
	server, err := NewServer(validLeaseReader(now), submitter, &fakeWindowReader{}, staticActivation{readiness.Blocked}, engineclock.NewFake(now), validScenario())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	receipt := publishReceipt(t, server, validPresenceRequest(now))
	requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "SCENARIO_BLOCKED")
	if len(submitter.inputs) != 0 {
		t.Fatalf("blocked scenario submitted %#v", submitter.inputs)
	}
}

func TestPublishMapsPresenceAndBusyToCanonicalObservation(t *testing.T) {
	now := ingressTestNow()
	tests := []struct {
		name    string
		request func(time.Time) *platformv1.PublishRequest
		want    func(time.Time) observation.Observation
	}{
		{
			name:    "person presence",
			request: validPresenceRequest,
			want: func(at time.Time) observation.Observation {
				payload := observation.PersonPresence{Present: true}
				return canonicalObservation(at, "presence-instance", 1, .8, &payload, nil)
			},
		},
		{
			name: "busy on call",
			request: func(at time.Time) *platformv1.PublishRequest {
				return validBusyRequest(at, platformv1.BusyReason_BUSY_REASON_ON_CALL)
			},
			want: func(at time.Time) observation.Observation {
				payload := observation.UserBusy{Busy: true, Reason: observation.BusyOnCall}
				return canonicalObservation(at, "busy-instance", 1, .7, nil, &payload)
			},
		},
		{
			name: "busy focused",
			request: func(at time.Time) *platformv1.PublishRequest {
				return validBusyRequest(at, platformv1.BusyReason_BUSY_REASON_FOCUSED)
			},
			want: func(at time.Time) observation.Observation {
				payload := observation.UserBusy{Busy: true, Reason: observation.BusyFocused}
				return canonicalObservation(at, "busy-instance", 1, .7, nil, &payload)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
			receipt := publishReceipt(t, server, test.request(now))
			requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED, "")
			if len(submitter.inputs) != 1 || !reflect.DeepEqual(submitter.inputs[0], test.want(now)) {
				t.Fatalf("submitted observations = %#v, want %#v", submitter.inputs, test.want(now))
			}
		})
	}
}

func TestPublishRejectsMalformedTransportWithoutSubmitting(t *testing.T) {
	now := ingressTestNow()
	tests := []struct {
		name   string
		mutate func(*platformv1.PublishRequest)
	}{
		{name: "nil request", mutate: nil},
		{name: "missing lease", mutate: func(r *platformv1.PublishRequest) { r.ProviderLeaseId = "" }},
		{name: "missing envelope", mutate: func(r *platformv1.PublishRequest) { r.Observation = nil }},
		{name: "missing id", mutate: func(r *platformv1.PublishRequest) { r.Observation.Id = "" }},
		{name: "missing source", mutate: func(r *platformv1.PublishRequest) { r.Observation.SourceId = "" }},
		{name: "zero source sequence", mutate: func(r *platformv1.PublishRequest) { r.Observation.SourceSeq = 0 }},
		{name: "missing subject", mutate: func(r *platformv1.PublishRequest) { r.Observation.SubjectId = "" }},
		{name: "missing trace", mutate: func(r *platformv1.PublishRequest) { r.Observation.TraceId = "" }},
		{name: "missing timestamp", mutate: func(r *platformv1.PublishRequest) { r.Observation.OccurredAt = nil }},
		{name: "invalid timestamp", mutate: func(r *platformv1.PublishRequest) {
			r.Observation.OccurredAt = &timestamppb.Timestamp{Seconds: 253402300800}
		}},
		{name: "missing ttl", mutate: func(r *platformv1.PublishRequest) { r.Observation.Ttl = nil }},
		{name: "zero ttl", mutate: func(r *platformv1.PublishRequest) { r.Observation.Ttl = durationpb.New(0) }},
		{name: "negative ttl", mutate: func(r *platformv1.PublishRequest) { r.Observation.Ttl = durationpb.New(-time.Second) }},
		{name: "invalid ttl", mutate: func(r *platformv1.PublishRequest) { r.Observation.Ttl = &durationpb.Duration{Seconds: 315576000001} }},
		{name: "negative confidence", mutate: func(r *platformv1.PublishRequest) { r.Observation.Confidence = -.1 }},
		{name: "high confidence", mutate: func(r *platformv1.PublishRequest) { r.Observation.Confidence = 1.1 }},
		{name: "nan confidence", mutate: func(r *platformv1.PublishRequest) { r.Observation.Confidence = float32(math.NaN()) }},
		{name: "infinite confidence", mutate: func(r *platformv1.PublishRequest) { r.Observation.Confidence = float32(math.Inf(1)) }},
		{name: "missing oneof", mutate: func(r *platformv1.PublishRequest) { r.Observation.Payload = nil }},
		{name: "nil payload message", mutate: func(r *platformv1.PublishRequest) {
			r.Observation.Payload = &platformv1.ObservationEnvelope_PersonPresence{}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
			var request *platformv1.PublishRequest
			if test.mutate != nil {
				request = validPresenceRequest(now)
				test.mutate(request)
			}
			response, err := server.Publish(context.Background(), request)
			requireStatusCode(t, err, codes.InvalidArgument)
			if response != nil || len(submitter.inputs) != 0 {
				t.Fatalf("Publish() = %#v and submitted %#v, want neither", response, submitter.inputs)
			}
		})
	}
}

func TestPublishRejectsInvalidBusyValuesWithoutSubmitting(t *testing.T) {
	now := ingressTestNow()
	tests := []struct {
		name   string
		busy   bool
		reason platformv1.BusyReason
	}{
		{name: "busy without reason", busy: true, reason: platformv1.BusyReason_BUSY_REASON_UNSPECIFIED},
		{name: "idle with reason", reason: platformv1.BusyReason_BUSY_REASON_ON_CALL},
		{name: "unknown enum", busy: true, reason: platformv1.BusyReason(99)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
			request := validBusyRequest(now, test.reason)
			request.Observation.GetUserBusy().Busy = test.busy
			response, err := server.Publish(context.Background(), request)
			requireStatusCode(t, err, codes.InvalidArgument)
			if response != nil || len(submitter.inputs) != 0 {
				t.Fatalf("Publish() = %#v and submitted %#v, want neither", response, submitter.inputs)
			}
		})
	}
}

func TestPublishRejectsDisallowedTransportPayloads(t *testing.T) {
	now := ingressTestNow()
	payloads := []struct {
		name   string
		mutate func(*platformv1.ObservationEnvelope)
	}{
		{name: "direct user reply", mutate: func(envelope *platformv1.ObservationEnvelope) {
			envelope.Payload = &platformv1.ObservationEnvelope_UserReply{UserReply: &platformv1.UserReply{}}
		}},
		{name: "user control", mutate: func(envelope *platformv1.ObservationEnvelope) {
			envelope.Payload = &platformv1.ObservationEnvelope_UserControl{UserControl: &platformv1.UserControl{Kind: platformv1.UserControlKind_USER_CONTROL_KIND_STOP}}
		}},
		{name: "quiet mode", mutate: func(envelope *platformv1.ObservationEnvelope) {
			envelope.Payload = &platformv1.ObservationEnvelope_QuietMode{QuietMode: &platformv1.QuietMode{Enabled: true}}
		}},
		{name: "device condition", mutate: func(envelope *platformv1.ObservationEnvelope) {
			envelope.Payload = &platformv1.ObservationEnvelope_DeviceCondition{DeviceCondition: &platformv1.DeviceCondition{DeviceId: "pc", Kind: platformv1.DeviceConditionKind_DEVICE_CONDITION_KIND_CONNECTED}}
		}},
	}
	for _, test := range payloads {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
			request := validPresenceRequest(now)
			test.mutate(request.Observation)
			receipt := publishReceipt(t, server, request)
			requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "PAYLOAD_NOT_ALLOWED")
			if len(submitter.inputs) != 0 {
				t.Fatalf("submitted observations = %#v, want none", submitter.inputs)
			}
		})
	}
}

func TestPublishEnforcesLeaseAndScenarioSelection(t *testing.T) {
	now := ingressTestNow()
	tests := []struct {
		name       string
		leaseID    string
		snapshot   readiness.ProviderSnapshot
		wantReason string
	}{
		{name: "lease not found", leaseID: "missing", wantReason: "LEASE_NOT_FOUND"},
		{name: "lease exact expiry", leaseID: "lease", snapshot: providerSnapshot("presence", "presence-instance", readiness.Healthy, now, readiness.PersonPresence), wantReason: "LEASE_EXPIRED"},
		{name: "unhealthy provider", leaseID: "lease", snapshot: providerSnapshot("presence", "presence-instance", readiness.Unhealthy, now.Add(time.Minute), readiness.PersonPresence), wantReason: "PROVIDER_UNHEALTHY"},
		{name: "provider not selected", leaseID: "lease", snapshot: providerSnapshot("other", "presence-instance", readiness.Healthy, now.Add(time.Minute), readiness.PersonPresence), wantReason: "PROVIDER_NOT_SELECTED"},
		{name: "provider instance mismatch", leaseID: "lease", snapshot: providerSnapshot("presence", "other-instance", readiness.Healthy, now.Add(time.Minute), readiness.PersonPresence), wantReason: "PROVIDER_INSTANCE_MISMATCH"},
		{name: "capability not declared", leaseID: "lease", snapshot: providerSnapshot("presence", "presence-instance", readiness.Healthy, now.Add(time.Minute), readiness.DeviceState), wantReason: "CAPABILITY_NOT_DECLARED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			leases := &fakeLeaseReader{snapshots: map[string]readiness.ProviderSnapshot{}}
			if test.snapshot.ProviderID != "" {
				leases.snapshots[test.leaseID] = test.snapshot
			}
			server := newTestServer(t, leases, submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
			request := validPresenceRequest(now)
			request.ProviderLeaseId = test.leaseID
			receipt := publishReceipt(t, server, request)
			requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, test.wantReason)
			if len(submitter.inputs) != 0 {
				t.Fatalf("submitted observations = %#v, want none", submitter.inputs)
			}
		})
	}
}

func TestPublishAppliesTTLBoundaryAndStableEngineErrors(t *testing.T) {
	now := ingressTestNow()
	t.Run("exact ttl expiry is accepted", func(t *testing.T) {
		submitter := &fakeSubmitter{}
		server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
		request := validPresenceRequest(now.Add(-time.Minute))
		request.Observation.Ttl = durationpb.New(time.Minute)
		requireReceipt(t, publishReceipt(t, server, request), platformv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED, "")
		if len(submitter.inputs) != 1 {
			t.Fatalf("submitted %d observations, want 1", len(submitter.inputs))
		}
	})

	t.Run("after ttl expiry is stale", func(t *testing.T) {
		submitter := &fakeSubmitter{}
		server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
		request := validPresenceRequest(now.Add(-time.Minute - time.Nanosecond))
		request.Observation.Ttl = durationpb.New(time.Minute)
		requireReceipt(t, publishReceipt(t, server, request), platformv1.ReceiptStatus_RECEIPT_STATUS_STALE, "TTL_EXPIRED")
		if len(submitter.inputs) != 0 {
			t.Fatalf("submitted observations = %#v, want none", submitter.inputs)
		}
	})

	t.Run("engine stale is not guessed duplicate", func(t *testing.T) {
		submitter := &fakeSubmitter{err: fault.New(fault.StaleInput, "test submit", errors.New("ambiguous stale input"))}
		server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
		receipt := publishReceipt(t, server, validPresenceRequest(now))
		requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_STALE, "ENGINE_STALE_INPUT")
		if receipt.Status == platformv1.ReceiptStatus_RECEIPT_STATUS_DUPLICATE {
			t.Fatal("ambiguous engine StaleInput was guessed as DUPLICATE")
		}
	})

	t.Run("engine unavailable is grpc unavailable", func(t *testing.T) {
		submitter := &fakeSubmitter{err: fault.New(fault.Unavailable, "test submit", errors.New("runner stopped"))}
		server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
		response, err := server.Publish(context.Background(), validPresenceRequest(now))
		requireStatusCode(t, err, codes.Unavailable)
		if response != nil {
			t.Fatalf("Publish() response = %#v, want nil", response)
		}
	})
}

func TestPublishHonorsCancelledContextWithoutSubmitting(t *testing.T) {
	now := ingressTestNow()
	submitter := &fakeSubmitter{}
	server := newTestServer(t, validLeaseReader(now), submitter, &fakeWindowReader{}, engineclock.NewFake(now), validScenario())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, err := server.Publish(ctx, validPresenceRequest(now))
	requireStatusCode(t, err, codes.Canceled)
	if response != nil || len(submitter.inputs) != 0 {
		t.Fatalf("Publish(cancelled) = %#v and submitted %#v, want neither", response, submitter.inputs)
	}
}

func newTestServer(t *testing.T, leases ProviderLeaseReader, submitter ObservationSubmitter, windows ReplyWindowReader, clock *engineclock.Fake, scenario readiness.ScenarioRequirements) *Server {
	t.Helper()
	server, err := NewServer(leases, submitter, windows, staticActivation{readiness.Ready}, clock, scenario)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func publishReceipt(t *testing.T, server *Server, request *platformv1.PublishRequest) *platformv1.ObservationReceipt {
	t.Helper()
	response, err := server.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if response == nil || response.Receipt == nil {
		t.Fatalf("Publish() response = %#v, want receipt", response)
	}
	if request != nil && request.Observation != nil && response.Receipt.ObservationId != request.Observation.Id {
		t.Fatalf("receipt observation id = %q, want %q", response.Receipt.ObservationId, request.Observation.Id)
	}
	return response.Receipt
}

func requireReceipt(t *testing.T, receipt *platformv1.ObservationReceipt, wantStatus platformv1.ReceiptStatus, wantReason string) {
	t.Helper()
	if receipt.Status != wantStatus || receipt.ReasonCode != wantReason {
		t.Fatalf("receipt = %#v, want status %s and reason %q", receipt, wantStatus, wantReason)
	}
}

func requireStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("status code = %s for %v, want %s", status.Code(err), err, want)
	}
}

func validPresenceRequest(at time.Time) *platformv1.PublishRequest {
	return &platformv1.PublishRequest{
		ProviderLeaseId: "lease-presence",
		Observation: &platformv1.ObservationEnvelope{
			Id:         "obs-presence",
			SourceId:   "presence-instance",
			SourceSeq:  1,
			OccurredAt: timestamppb.New(at),
			Ttl:        durationpb.New(time.Minute),
			SubjectId:  "user-1",
			Confidence: .8,
			TraceId:    "trace-presence",
			Payload: &platformv1.ObservationEnvelope_PersonPresence{
				PersonPresence: &platformv1.PersonPresence{Present: true},
			},
		},
	}
}

func validBusyRequest(at time.Time, reason platformv1.BusyReason) *platformv1.PublishRequest {
	request := validPresenceRequest(at)
	request.ProviderLeaseId = "lease-busy"
	request.Observation.Id = "obs-busy"
	request.Observation.SourceId = "busy-instance"
	request.Observation.Confidence = .7
	request.Observation.TraceId = "trace-busy"
	request.Observation.Payload = &platformv1.ObservationEnvelope_UserBusy{
		UserBusy: &platformv1.UserBusy{Busy: true, Reason: reason},
	}
	return request
}

func canonicalObservation(at time.Time, sourceID string, sequence uint64, confidence float32, presence *observation.PersonPresence, busy *observation.UserBusy) observation.Observation {
	input := observation.Observation{
		ID:             "obs-presence",
		SourceID:       sourceID,
		SourceSeq:      sequence,
		OccurredAt:     at,
		TTL:            time.Minute,
		SubjectID:      "user-1",
		Confidence:     confidence,
		TraceID:        "trace-presence",
		PersonPresence: presence,
		UserBusy:       busy,
	}
	if busy != nil {
		input.ID = "obs-busy"
		input.TraceID = "trace-busy"
	}
	return input
}

func validScenario() readiness.ScenarioRequirements {
	return readiness.ScenarioRequirements{
		ID:                       "ingress-test",
		MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous,
		Required: []readiness.CapabilityRequirement{
			{Kind: readiness.PersonPresence, ProviderID: "presence", Compatibility: ingressTestCompatibility()},
			{Kind: readiness.BusyState, ProviderID: "busy", Compatibility: ingressTestCompatibility()},
			{Kind: readiness.VoiceActivity, ProviderID: "vad", Compatibility: ingressTestCompatibility()},
		},
	}
}

func ingressTestCompatibility() readiness.ProviderCompatibility {
	return readiness.ProviderCompatibility{
		ProtocolVersion:              "v1",
		AllowedPrivacyClasses:        []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal},
		MaximumLatency:               time.Second,
		AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
	}
}

func validLeaseReader(now time.Time) *fakeLeaseReader {
	return &fakeLeaseReader{snapshots: map[string]readiness.ProviderSnapshot{
		"lease-presence": providerSnapshot("presence", "presence-instance", readiness.Healthy, now.Add(time.Minute), readiness.PersonPresence),
		"lease-busy":     providerSnapshot("busy", "busy-instance", readiness.Healthy, now.Add(time.Minute), readiness.BusyState),
		"lease-vad":      providerSnapshot("vad", "vad-instance", readiness.Healthy, now.Add(time.Minute), readiness.VoiceActivity),
	}}
}

func providerSnapshot(providerID, instanceID string, health readiness.ProviderHealth, expiresAt time.Time, capabilities ...readiness.CapabilityKind) readiness.ProviderSnapshot {
	return readiness.ProviderSnapshot{
		ProviderID:            providerID,
		InstanceID:            instanceID,
		ProtocolVersion:       "v1",
		ImplementationVersion: "test-v1",
		Capabilities:          capabilities,
		Health:                health,
		LeaseExpiresAt:        expiresAt,
		OperationalProfile: readiness.ProviderOperationalProfile{
			PrivacyClass:          readiness.ProviderPrivacyDeviceLocal,
			MaximumLatency:        100 * time.Millisecond,
			CancellationSemantics: readiness.ProviderCancellationCooperative,
		},
	}
}

func ingressTestNow() time.Time {
	return time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
}

type fakeLeaseReader struct {
	snapshots map[string]readiness.ProviderSnapshot
}

type staticActivation struct{ status readiness.ActivationStatus }

func (s staticActivation) CurrentActivation() readiness.Activation {
	return readiness.Activation{Status: s.status}
}

func (f *fakeLeaseReader) LeaseSnapshot(leaseID string) (readiness.ProviderSnapshot, bool) {
	snapshot, ok := f.snapshots[leaseID]
	return snapshot, ok
}

type fakeSubmitter struct {
	inputs []observation.Observation
	result application.Result
	err    error
}

func (f *fakeSubmitter) SubmitObservation(_ context.Context, input observation.Observation) (application.Result, error) {
	f.inputs = append(f.inputs, input)
	return f.result, f.err
}

type fakeWindowReader struct {
	window application.ReplyAcceptanceWindow
	open   bool
}

func (f *fakeWindowReader) CurrentReplyAcceptanceWindow() (application.ReplyAcceptanceWindow, bool) {
	return f.window, f.open
}
