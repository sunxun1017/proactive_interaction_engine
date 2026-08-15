package ingress

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/readiness"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMapFaceIdentificationEvidence(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishFaceIdentificationEvidenceRequest{
		Metadata:      validMetadata(now),
		FacesObserved: 1,
		Candidates: []*platformv1.FaceIdentificationCandidate{
			{CandidateId: "face-1", ProfileRef: "profile-a", Score: 0.91, ModelVersion: "face.v1", Liveness: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_PASSED},
		},
	}
	mapped, err := mapFaceIdentification(request, now)
	if err != nil {
		t.Fatalf("mapFaceIdentification() error = %v", err)
	}
	if mapped.capability != readiness.FaceIdentification || mapped.metadata.fragmentID != "fragment-1" || mapped.metadata.evidenceWindowID != "window-1" || mapped.metadata.providerLeaseID != "lease-1" || mapped.metadata.sourceInstanceID != "face-instance" || mapped.metadata.sourceSeq != 7 || mapped.metadata.traceID != "trace-1" {
		t.Fatalf("mapped metadata = %#v", mapped)
	}
	wantCapabilities := []readiness.CapabilityKind{
		readiness.FaceIdentification,
		readiness.FaceDetection,
		readiness.FaceLiveness,
	}
	if !reflect.DeepEqual(mapped.requiredCapabilities, wantCapabilities) {
		t.Fatalf("required capabilities = %v, want %v", mapped.requiredCapabilities, wantCapabilities)
	}
	if mapped.metadata.occurredAt != now.Add(-time.Second) || mapped.metadata.expiresAt != now.Add(time.Second) {
		t.Fatalf("mapped timing = %#v", mapped.metadata)
	}
	want := identity.Evidence{FacesObserved: 1, FaceIdentifications: []identity.FaceIdentificationCandidate{{
		ID: "face-1", ProfileRef: "profile-a", Score: 0.91,
		ModelVersion: "face.v1", OccurredAt: now.Add(-time.Second), Liveness: identity.LivenessPassed,
	}}}
	if !reflect.DeepEqual(mapped.evidence, want) {
		t.Fatalf("mapped evidence = %#v, want %#v", mapped.evidence, want)
	}
}

func TestMapSpeakerIdentificationEvidence(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: validMetadata(now),
		Candidates: []*platformv1.SpeakerIdentificationCandidate{
			{CandidateId: "speaker-1", ProfileRef: "profile-a", Score: 0.82, ModelVersion: "speaker.v1"},
		},
	}
	mapped, err := mapSpeakerIdentification(request, now)
	if err != nil {
		t.Fatalf("mapSpeakerIdentification() error = %v", err)
	}
	if mapped.capability != readiness.SpeakerIdentification || len(mapped.evidence.SpeakerIdentifications) != 1 {
		t.Fatalf("mapped = %#v", mapped)
	}
	if want := []readiness.CapabilityKind{readiness.SpeakerIdentification}; !reflect.DeepEqual(mapped.requiredCapabilities, want) {
		t.Fatalf("required capabilities = %v, want %v", mapped.requiredCapabilities, want)
	}
	want := identity.SpeakerIdentificationCandidate{
		ID: "speaker-1", ProfileRef: "profile-a", Score: 0.82,
		ModelVersion: "speaker.v1", OccurredAt: now.Add(-time.Second),
	}
	if !reflect.DeepEqual(mapped.evidence.SpeakerIdentifications[0], want) {
		t.Fatalf("candidate = %#v, want %#v", mapped.evidence.SpeakerIdentifications[0], want)
	}
}

func TestMapSpeakerVerificationKeepsExpectedProfileApplicationOwned(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishSpeakerVerificationEvidenceRequest{
		Metadata: validMetadata(now), VerificationChallengeId: "challenge-1",
		CandidateId: "verification-1", Score: 0.94, ModelVersion: "verification.v1",
	}
	mapped, err := mapSpeakerVerification(request, now)
	if err != nil {
		t.Fatalf("mapSpeakerVerification() error = %v", err)
	}
	if mapped.capability != readiness.SpeakerVerification || mapped.challengeID != "challenge-1" || mapped.candidate.ID != "verification-1" || mapped.candidate.ProfileRef != "" || mapped.candidate.Score != 0.94 || mapped.candidate.ModelVersion != "verification.v1" || mapped.candidate.OccurredAt != now.Add(-time.Second) {
		t.Fatalf("mapped verification = %#v", mapped)
	}
	if want := []readiness.CapabilityKind{readiness.SpeakerVerification}; !reflect.DeepEqual(mapped.requiredCapabilities, want) {
		t.Fatalf("required capabilities = %v, want %v", mapped.requiredCapabilities, want)
	}
	descriptor := request.ProtoReflect().Descriptor()
	if field := descriptor.Fields().ByName("profile_ref"); field != nil {
		t.Fatal("speaker verification wire contract lets worker choose profile_ref")
	}
}

func TestMapFaceIdentificationRequiresLivenessCapabilityOnlyWhenUsed(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishFaceIdentificationEvidenceRequest{
		Metadata:      validMetadata(now),
		FacesObserved: 1,
		Candidates: []*platformv1.FaceIdentificationCandidate{
			{CandidateId: "face-1", ProfileRef: "profile-a", Score: 0.91, ModelVersion: "face.v1", Liveness: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN},
		},
	}
	mapped, err := mapFaceIdentification(request, now)
	if err != nil {
		t.Fatalf("mapFaceIdentification() error = %v", err)
	}
	want := []readiness.CapabilityKind{readiness.FaceIdentification, readiness.FaceDetection}
	if !reflect.DeepEqual(mapped.requiredCapabilities, want) {
		t.Fatalf("required capabilities = %v, want %v", mapped.requiredCapabilities, want)
	}
}

func TestEvidenceMappersRejectMalformedOrStaleInput(t *testing.T) {
	now := mapperNow()
	validFace := func() *platformv1.PublishFaceIdentificationEvidenceRequest {
		return &platformv1.PublishFaceIdentificationEvidenceRequest{
			Metadata: validMetadata(now), FacesObserved: 1,
			Candidates: []*platformv1.FaceIdentificationCandidate{
				{CandidateId: "face-1", ProfileRef: "profile-a", Score: 0.9, ModelVersion: "face.v1", Liveness: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN},
			},
		}
	}
	tests := []struct {
		name   string
		mutate func(*platformv1.PublishFaceIdentificationEvidenceRequest)
	}{
		{name: "missing metadata", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Metadata = nil }},
		{name: "blank fragment id", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Metadata.FragmentId = "" }},
		{name: "blank evidence window", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Metadata.EvidenceWindowId = "" }},
		{name: "whitespace source", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.SourceInstanceId = " face"
		}},
		{name: "zero source sequence", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Metadata.SourceSeq = 0 }},
		{name: "invalid timestamp", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.OccurredAt = &timestamppb.Timestamp{Seconds: 253402300800}
		}},
		{name: "future timestamp", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.OccurredAt = timestamppb.New(now.Add(time.Nanosecond))
		}},
		{name: "zero ttl", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.Ttl = durationpb.New(0)
		}},
		{name: "ttl above maximum", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.Ttl = durationpb.New(maxEvidenceTTL + time.Nanosecond)
		}},
		{name: "stale ttl", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.Ttl = durationpb.New(time.Second)
		}},
		{name: "identifier too long", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.FragmentId = strings.Repeat("f", maxIdentifierLength+1)
		}},
		{name: "trace too long", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Metadata.TraceId = strings.Repeat("t", maxTraceIDLength+1)
		}},
		{name: "zero faces", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.FacesObserved = 0 }},
		{name: "no candidates", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Candidates = nil }},
		{name: "nil candidate", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Candidates[0] = nil }},
		{name: "duplicate candidate", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates = append(value.Candidates, value.Candidates[0])
		}},
		{name: "mixed model versions", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates = append(value.Candidates, &platformv1.FaceIdentificationCandidate{
				CandidateId: "face-2", ProfileRef: "profile-b", Score: 0.8, ModelVersion: "face.v2", Liveness: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN,
			})
		}},
		{name: "NaN score", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates[0].Score = math.NaN()
		}},
		{name: "score above one", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Candidates[0].Score = 1.01 }},
		{name: "unspecified liveness", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates[0].Liveness = platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNSPECIFIED
		}},
		{name: "unknown liveness enum", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates[0].Liveness = platformv1.FaceLivenessState(99)
		}},
		{name: "too many candidates", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates = make([]*platformv1.FaceIdentificationCandidate, maxCandidates+1)
			for index := range value.Candidates {
				value.Candidates[index] = &platformv1.FaceIdentificationCandidate{CandidateId: string(rune('a' + index)), ProfileRef: "profile-a", Score: 0.5, ModelVersion: "face.v1", Liveness: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN}
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validFace()
			test.mutate(request)
			if _, err := mapFaceIdentification(request, now); err == nil {
				t.Fatal("mapFaceIdentification() error = nil")
			}
		})
	}
}

func TestEvidenceMapperAcceptsExactTTLAndStringLimits(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: &platformv1.IdentityEvidenceMetadata{
			FragmentId: strings.Repeat("f", maxIdentifierLength), ProviderLeaseId: strings.Repeat("l", maxIdentifierLength),
			SourceInstanceId: strings.Repeat("s", maxIdentifierLength), EvidenceWindowId: strings.Repeat("w", maxIdentifierLength),
			SourceSeq: 1, OccurredAt: timestamppb.New(now), Ttl: durationpb.New(maxEvidenceTTL), TraceId: strings.Repeat("t", maxTraceIDLength),
		},
		Candidates: []*platformv1.SpeakerIdentificationCandidate{{
			CandidateId: strings.Repeat("c", maxIdentifierLength), ProfileRef: strings.Repeat("p", maxIdentifierLength),
			Score: 0.5, ModelVersion: strings.Repeat("m", maxModelVersionLength),
		}},
	}
	if _, err := mapSpeakerIdentification(request, now); err != nil {
		t.Fatalf("mapSpeakerIdentification() exact limits error = %v", err)
	}
}

func TestSpeakerMappersRejectEmptyCandidatesAndVerificationFields(t *testing.T) {
	now := mapperNow()
	if _, err := mapSpeakerIdentification(&platformv1.PublishSpeakerIdentificationEvidenceRequest{Metadata: validMetadata(now)}, now); err == nil {
		t.Fatal("mapSpeakerIdentification(empty) error = nil")
	}
	valid := &platformv1.PublishSpeakerVerificationEvidenceRequest{
		Metadata: validMetadata(now), VerificationChallengeId: "challenge-1",
		CandidateId: "candidate-1", Score: 0.8, ModelVersion: "speaker.v1",
	}
	for _, mutate := range []func(*platformv1.PublishSpeakerVerificationEvidenceRequest){
		func(value *platformv1.PublishSpeakerVerificationEvidenceRequest) { value.VerificationChallengeId = "" },
		func(value *platformv1.PublishSpeakerVerificationEvidenceRequest) { value.CandidateId = " candidate" },
		func(value *platformv1.PublishSpeakerVerificationEvidenceRequest) { value.ModelVersion = "" },
		func(value *platformv1.PublishSpeakerVerificationEvidenceRequest) { value.Score = math.Inf(1) },
	} {
		request := proto.Clone(valid).(*platformv1.PublishSpeakerVerificationEvidenceRequest)
		mutate(request)
		if _, err := mapSpeakerVerification(request, now); err == nil {
			t.Fatal("mapSpeakerVerification() error = nil")
		}
	}
}

func TestIdentityContractContainsNoRawOrUntypedPayload(t *testing.T) {
	file := (&platformv1.PublishFaceIdentificationEvidenceRequest{}).ProtoReflect().Descriptor().ParentFile()
	messages := file.Messages()
	for messageIndex := 0; messageIndex < messages.Len(); messageIndex++ {
		message := messages.Get(messageIndex)
		fields := message.Fields()
		for index := 0; index < fields.Len(); index++ {
			field := fields.Get(index)
			isAny := field.Kind() == protoreflect.MessageKind && field.Message().FullName() == "google.protobuf.Any"
			if field.Kind() == protoreflect.BytesKind || field.Kind() == protoreflect.GroupKind || field.IsMap() || isAny || string(field.Name()) == "payload" {
				t.Fatalf("%s contains forbidden field %s (%s)", message.FullName(), field.Name(), field.Kind())
			}
		}
	}
}

func validMetadata(now time.Time) *platformv1.IdentityEvidenceMetadata {
	return &platformv1.IdentityEvidenceMetadata{
		FragmentId: "fragment-1", ProviderLeaseId: "lease-1", SourceInstanceId: "face-instance",
		SourceSeq: 7, OccurredAt: timestamppb.New(now.Add(-time.Second)),
		Ttl: durationpb.New(2 * time.Second), TraceId: "trace-1", EvidenceWindowId: "window-1",
	}
}

func mapperNow() time.Time {
	return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
}
