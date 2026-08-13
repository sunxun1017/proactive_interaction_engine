package ingress

import (
	"reflect"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestPublishGatesSpeechAgainstApplicationReplyWindow(t *testing.T) {
	now := ingressTestNow()
	window := application.ReplyAcceptanceWindow{
		SubjectID: "user-1",
		OpenedAt:  now,
		Deadline:  now.Add(8 * time.Second),
	}
	tests := []struct {
		name       string
		at         time.Time
		subjectID  string
		windowOpen bool
		active     bool
		wantReason string
	}{
		{name: "inactive", at: now, subjectID: "user-1", windowOpen: true, wantReason: "INACTIVE_SPEECH"},
		{name: "no window", at: now, subjectID: "user-1", active: true, wantReason: "REPLY_WINDOW_CLOSED"},
		{name: "wrong subject", at: now, subjectID: "other", windowOpen: true, active: true, wantReason: "REPLY_WINDOW_CLOSED"},
		{name: "before open", at: now.Add(-time.Nanosecond), subjectID: "user-1", windowOpen: true, active: true, wantReason: "REPLY_WINDOW_CLOSED"},
		{name: "exact deadline", at: now.Add(8 * time.Second), subjectID: "user-1", windowOpen: true, active: true, wantReason: "REPLY_WINDOW_CLOSED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			windows := &fakeWindowReader{window: window, open: test.windowOpen}
			server := newTestServer(t, validLeaseReader(now), submitter, windows, engineclock.NewFake(now), validScenario())
			request := validSpeechRequest(test.at, test.subjectID, test.active, false)
			receipt := publishReceipt(t, server, request)
			requireReceipt(t, receipt, platformv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, test.wantReason)
			if len(submitter.inputs) != 0 {
				t.Fatalf("submitted observations = %#v, want none", submitter.inputs)
			}
		})
	}
}

func TestPublishMapsSpeechInsideHalfOpenWindowToCanonicalUserReply(t *testing.T) {
	now := ingressTestNow()
	window := application.ReplyAcceptanceWindow{SubjectID: "user-1", OpenedAt: now, Deadline: now.Add(8 * time.Second)}
	for _, test := range []struct {
		name            string
		at              time.Time
		addressingAgent bool
	}{
		{name: "at open ignores false addressing", at: window.OpenedAt},
		{name: "before deadline ignores true addressing", at: window.Deadline.Add(-time.Nanosecond), addressingAgent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			submitter := &fakeSubmitter{}
			server := newTestServer(
				t,
				validLeaseReader(now),
				submitter,
				&fakeWindowReader{window: window, open: true},
				engineclock.NewFake(now),
				validScenario(),
			)
			request := validSpeechRequest(test.at, "user-1", true, test.addressingAgent)
			request.Observation.Confidence = .23
			requireReceipt(t, publishReceipt(t, server, request), platformv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED, "")

			reply := observation.UserReply{}
			want := observation.Observation{
				ID:         "obs-speech",
				SourceID:   "vad-instance",
				SourceSeq:  1,
				OccurredAt: test.at,
				TTL:        time.Minute,
				SubjectID:  "user-1",
				Confidence: 1,
				TraceID:    "trace-speech",
				UserReply:  &reply,
			}
			if len(submitter.inputs) != 1 || !reflect.DeepEqual(submitter.inputs[0], want) {
				t.Fatalf("submitted observations = %#v, want %#v", submitter.inputs, want)
			}
		})
	}
}

func validSpeechRequest(at time.Time, subjectID string, active, addressingAgent bool) *platformv1.PublishRequest {
	request := validPresenceRequest(at)
	request.ProviderLeaseId = "lease-vad"
	request.Observation.Id = "obs-speech"
	request.Observation.SourceId = "vad-instance"
	request.Observation.SubjectId = subjectID
	request.Observation.Confidence = .6
	request.Observation.TraceId = "trace-speech"
	request.Observation.Payload = &platformv1.ObservationEnvelope_SpeechActivity{
		SpeechActivity: &platformv1.SpeechActivity{Active: active, AddressingAgent: addressingAgent},
	}
	return request
}
