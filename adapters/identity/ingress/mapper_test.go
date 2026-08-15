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
	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMapFaceIdentificationEvidence(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishFaceIdentificationEvidenceRequest{
		Metadata: validMetadata(now),
		Candidates: []*platformv1.FaceIdentificationCandidate{
			{CandidateId: "face-1", ProfileRef: "profile-a", Score: 0.91, ModelVersion: "face.v1"},
		},
	}
	mapped, err := mapFaceIdentification(request, now)
	if err != nil {
		t.Fatalf("mapFaceIdentification() error = %v", err)
	}
	if mapped.capability != readiness.FaceIdentification || mapped.metadata.fragmentID != "fragment-1" || mapped.metadata.evidenceWindowID != "window-1" || mapped.metadata.providerLeaseID != "lease-1" || mapped.metadata.sourceInstanceID != "face-instance" || mapped.metadata.sourceSeq != 7 || mapped.metadata.traceID != "trace-1" {
		t.Fatalf("mapped metadata = %#v", mapped)
	}
	if mapped.metadata.occurredAt != now.Add(-time.Second) || mapped.metadata.expiresAt != now.Add(time.Second) {
		t.Fatalf("mapped timing = %#v", mapped.metadata)
	}
	want := []identity.FaceIdentificationCandidate{{
		ID: "face-1", ProfileRef: "profile-a", Score: 0.91,
		ModelVersion: "face.v1",
	}}
	if !reflect.DeepEqual(mapped.candidates, want) {
		t.Fatalf("mapped candidates = %#v, want %#v", mapped.candidates, want)
	}
}

func TestMapFaceDetectionEvidenceKeepsZeroAndMultipleCounts(t *testing.T) {
	now := mapperNow()
	for _, count := range []uint32{0, 2} {
		request := &platformv1.PublishFaceDetectionEvidenceRequest{
			Metadata: validMetadata(now), FacesObserved: count,
		}
		mapped, err := mapFaceDetection(request, now)
		if err != nil {
			t.Fatalf("mapFaceDetection(%d) error = %v", count, err)
		}
		if mapped.capability != readiness.FaceDetection || mapped.facesObserved != count || mapped.metadata.fragmentID != "fragment-1" {
			t.Fatalf("mapFaceDetection(%d) = %#v", count, mapped)
		}
	}
}

func TestMapFaceLivenessEvidence(t *testing.T) {
	now := mapperNow()
	tests := []struct {
		wire platformv1.FaceLivenessState
		want identity.Liveness
	}{
		{wire: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN, want: identity.LivenessUnknown},
		{wire: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_PASSED, want: identity.LivenessPassed},
		{wire: platformv1.FaceLivenessState_FACE_LIVENESS_STATE_FAILED, want: identity.LivenessFailed},
	}
	for _, test := range tests {
		request := &platformv1.PublishFaceLivenessEvidenceRequest{Metadata: validMetadata(now), State: test.wire}
		mapped, err := mapFaceLiveness(request, now)
		if err != nil {
			t.Fatalf("mapFaceLiveness(%s) error = %v", test.wire, err)
		}
		if mapped.capability != readiness.FaceLiveness || mapped.state != test.want {
			t.Fatalf("mapFaceLiveness(%s) = %#v, want %s", test.wire, mapped, test.want)
		}
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
	if mapped.capability != readiness.SpeakerIdentification || len(mapped.candidates) != 1 {
		t.Fatalf("mapped = %#v", mapped)
	}
	want := identity.SpeakerIdentificationCandidate{
		ID: "speaker-1", ProfileRef: "profile-a", Score: 0.82,
		ModelVersion: "speaker.v1",
	}
	if !reflect.DeepEqual(mapped.candidates[0], want) {
		t.Fatalf("candidate = %#v, want %#v", mapped.candidates[0], want)
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
	if mapped.capability != readiness.SpeakerVerification || mapped.challengeID != "challenge-1" || mapped.candidate.ID != "verification-1" || mapped.candidate.Score != 0.94 || mapped.candidate.ModelVersion != "verification.v1" {
		t.Fatalf("mapped verification = %#v", mapped)
	}
	descriptor := request.ProtoReflect().Descriptor()
	if field := descriptor.Fields().ByName("profile_ref"); field != nil {
		t.Fatal("speaker verification wire contract lets worker choose profile_ref")
	}
}

func TestIdentificationMappersAcceptExplicitNoMatch(t *testing.T) {
	now := mapperNow()
	face, err := mapFaceIdentification(&platformv1.PublishFaceIdentificationEvidenceRequest{Metadata: validMetadata(now)}, now)
	if err != nil || face.capability != readiness.FaceIdentification || len(face.candidates) != 0 {
		t.Fatalf("mapFaceIdentification(empty) = %#v, %v", face, err)
	}
	speaker, err := mapSpeakerIdentification(&platformv1.PublishSpeakerIdentificationEvidenceRequest{Metadata: validMetadata(now)}, now)
	if err != nil || speaker.capability != readiness.SpeakerIdentification || len(speaker.candidates) != 0 {
		t.Fatalf("mapSpeakerIdentification(empty) = %#v, %v", speaker, err)
	}
}

func TestEvidenceMappersRejectMalformedOrStaleInput(t *testing.T) {
	now := mapperNow()
	validFace := func() *platformv1.PublishFaceIdentificationEvidenceRequest {
		return &platformv1.PublishFaceIdentificationEvidenceRequest{
			Metadata: validMetadata(now),
			Candidates: []*platformv1.FaceIdentificationCandidate{
				{CandidateId: "face-1", ProfileRef: "profile-a", Score: 0.9, ModelVersion: "face.v1"},
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
		{name: "nil candidate", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Candidates[0] = nil }},
		{name: "duplicate candidate", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates = append(value.Candidates, value.Candidates[0])
		}},
		{name: "mixed model versions", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates = append(value.Candidates, &platformv1.FaceIdentificationCandidate{
				CandidateId: "face-2", ProfileRef: "profile-b", Score: 0.8, ModelVersion: "face.v2",
			})
		}},
		{name: "NaN score", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates[0].Score = math.NaN()
		}},
		{name: "score above one", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) { value.Candidates[0].Score = 1.01 }},
		{name: "too many candidates", mutate: func(value *platformv1.PublishFaceIdentificationEvidenceRequest) {
			value.Candidates = make([]*platformv1.FaceIdentificationCandidate, maxCandidates+1)
			for index := range value.Candidates {
				value.Candidates[index] = &platformv1.FaceIdentificationCandidate{CandidateId: string(rune('a' + index)), ProfileRef: "profile-a", Score: 0.5, ModelVersion: "face.v1"}
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

func TestEvidenceMapperClassifiesExpiredTTLAsStale(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishFaceDetectionEvidenceRequest{Metadata: validMetadata(now)}
	request.Metadata.Ttl = durationpb.New(time.Second)
	if _, err := mapFaceDetection(request, now); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("mapFaceDetection(expired) error = %v, want StaleInput", err)
	}
}

func TestFaceEvidenceMappersRejectInvalidInput(t *testing.T) {
	now := mapperNow()
	if _, err := mapFaceDetection(nil, now); err == nil {
		t.Fatal("mapFaceDetection(nil) error = nil")
	}
	for _, state := range []platformv1.FaceLivenessState{
		platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNSPECIFIED,
		platformv1.FaceLivenessState(99),
	} {
		request := &platformv1.PublishFaceLivenessEvidenceRequest{Metadata: validMetadata(now), State: state}
		if _, err := mapFaceLiveness(request, now); err == nil {
			t.Fatalf("mapFaceLiveness(%d) error = nil", state)
		}
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

func TestIdentificationMapperAcceptsMaximumCandidates(t *testing.T) {
	now := mapperNow()
	request := &platformv1.PublishSpeakerIdentificationEvidenceRequest{
		Metadata: validMetadata(now), Candidates: make([]*platformv1.SpeakerIdentificationCandidate, maxCandidates),
	}
	for index := range request.Candidates {
		request.Candidates[index] = &platformv1.SpeakerIdentificationCandidate{
			CandidateId: string(rune('a' + index)), ProfileRef: "profile-a", Score: 0.5, ModelVersion: "speaker.v1",
		}
	}
	if mapped, err := mapSpeakerIdentification(request, now); err != nil || len(mapped.candidates) != maxCandidates {
		t.Fatalf("mapSpeakerIdentification(max) = %#v, %v", mapped, err)
	}
}

func TestSpeakerVerificationMapperRejectsInvalidFields(t *testing.T) {
	now := mapperNow()
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

func TestIdentityContractSeparatesFaceCapabilitiesAndReservesRemovedFields(t *testing.T) {
	file := (&platformv1.PublishFaceIdentificationEvidenceRequest{}).ProtoReflect().Descriptor().ParentFile()
	service := file.Services().ByName("IdentityEvidenceIngressService")
	for _, name := range []protoreflect.Name{
		"PublishFaceDetectionEvidence",
		"PublishFaceIdentificationEvidence",
		"PublishFaceLivenessEvidence",
		"PublishSpeakerIdentificationEvidence",
		"PublishSpeakerVerificationEvidence",
	} {
		if service == nil || service.Methods().ByName(name) == nil {
			t.Fatalf("identity service is missing %s", name)
		}
	}

	faceRequest := (&platformv1.PublishFaceIdentificationEvidenceRequest{}).ProtoReflect().Descriptor()
	if faceRequest.Fields().ByNumber(1) == nil || faceRequest.Fields().ByNumber(3) == nil || faceRequest.Fields().ByNumber(2) != nil || !reservedNumber(faceRequest.ReservedRanges(), 2) {
		t.Fatalf("face identification request fields/reservations = %v/%v", faceRequest.Fields(), faceRequest.ReservedRanges())
	}
	faceCandidate := (&platformv1.FaceIdentificationCandidate{}).ProtoReflect().Descriptor()
	if faceCandidate.Fields().ByNumber(5) != nil || !reservedNumber(faceCandidate.ReservedRanges(), 5) {
		t.Fatalf("face candidate field 5 reservation = %v/%v", faceCandidate.Fields().ByNumber(5), faceCandidate.ReservedRanges())
	}
	detection := (&platformv1.PublishFaceDetectionEvidenceRequest{}).ProtoReflect().Descriptor()
	if detection.Fields().ByName("metadata").Number() != 1 || detection.Fields().ByName("faces_observed").Number() != 2 {
		t.Fatalf("face detection fields = %v", detection.Fields())
	}
	liveness := (&platformv1.PublishFaceLivenessEvidenceRequest{}).ProtoReflect().Descriptor()
	if liveness.Fields().ByName("metadata").Number() != 1 || liveness.Fields().ByName("state").Number() != 2 {
		t.Fatalf("face liveness fields = %v", liveness.Fields())
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

func reservedNumber(ranges protoreflect.FieldRanges, number protoreflect.FieldNumber) bool {
	for index := 0; index < ranges.Len(); index++ {
		current := ranges.Get(index)
		if number >= current[0] && number < current[1] {
			return true
		}
	}
	return false
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
