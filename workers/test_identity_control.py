import datetime
import threading
import unittest
import uuid

import grpc
from google.protobuf import timestamp_pb2
from proactive.platform.v1 import capability_pb2, identity_pb2, identity_worker_pb2

from workers.identity_control import (
    EvidenceCandidate,
    IdentityEvidencePublisher,
    LatestIdentityWork,
    PublishDisposition,
    VerificationCandidate,
)


UTC = datetime.timezone.utc


class LatestIdentityWorkTest(unittest.TestCase):
    def setUp(self):
        self.now = datetime.datetime(2026, 8, 15, 10, 0, tzinfo=UTC)

    def test_vision_claimed_bytes_have_executor_ownership_until_release(self):
        latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(vision_active(2, self.now, b"secret-face"))

        work = latest.claim()
        self.assertEqual(work.revision, 2)
        encoded = work.identification_templates[0].encoded
        self.assertEqual(bytes(encoded), b"secret-face")
        self.assertTrue(latest.is_current(work))

        latest.apply(identity_worker_pb2.WatchVisionResponse(
            revision=3, idle=identity_worker_pb2.IdentityWorkerIdle()
        ))
        self.assertFalse(latest.is_current(work))
        self.assertEqual(bytes(encoded), b"secret-face")
        latest.release(work)
        self.assertEqual(bytes(encoded), bytes(len(b"secret-face")))

    def test_idle_and_reconnect_do_not_mutate_claimed_executor_input(self):
        latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(vision_active(1, self.now, b"claimed-secret"))
        work = latest.claim()
        encoded = work.identification_templates[0].encoded
        completed = threading.Event()

        def replace_control_state():
            latest.apply(identity_worker_pb2.WatchVisionResponse(
                revision=2, idle=identity_worker_pb2.IdentityWorkerIdle()
            ))
            latest.begin_stream()
            completed.set()

        thread = threading.Thread(target=replace_control_state)
        thread.start()
        thread.join(1)
        self.assertTrue(completed.is_set())
        self.assertEqual(bytes(encoded), b"claimed-secret")
        self.assertFalse(latest.is_current(work))
        latest.release(work)
        self.assertEqual(bytes(encoded), bytes(len(b"claimed-secret")))

    def test_rejects_non_increasing_missing_state_and_wrong_stack(self):
        latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(identity_worker_pb2.WatchVisionResponse(
            revision=1, idle=identity_worker_pb2.IdentityWorkerIdle()
        ))
        for snapshot in (
            identity_worker_pb2.WatchVisionResponse(
                revision=1, idle=identity_worker_pb2.IdentityWorkerIdle()
            ),
            identity_worker_pb2.WatchVisionResponse(revision=2),
            identity_worker_pb2.WatchAudioResponse(
                revision=2, idle=identity_worker_pb2.IdentityWorkerIdle()
            ),
        ):
            with self.subTest(snapshot=snapshot):
                with self.assertRaises(ValueError):
                    latest.apply(snapshot)

    def test_exact_deadline_discards_active_and_reconnect_starts_idle(self):
        latest = LatestIdentityWork("audio", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(audio_identification(
            1, self.now - datetime.timedelta(seconds=1), self.now
        ))
        self.assertIsNone(latest.claim())

        latest.apply(audio_identification(2, self.now, self.now + datetime.timedelta(seconds=1)))
        work = latest.claim()
        self.assertIsNotNone(work)
        latest.begin_stream()
        self.assertFalse(latest.is_current(work))
        self.assertIsNone(latest.claim())


class IdentityEvidencePublisherTest(unittest.TestCase):
    def setUp(self):
        self.now = datetime.datetime(2026, 8, 15, 11, 0, tzinfo=UTC)
        self.stub = FakeIdentityStub()
        self.sessions = {
            capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION: FakeEvidenceSession(
                capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION, "lease-detection"
            ),
            capability_pb2.SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION: FakeEvidenceSession(
                capability_pb2.SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION, "lease-face"
            ),
            capability_pb2.SERVICE_CAPABILITY_KIND_FACE_LIVENESS: FakeEvidenceSession(
                capability_pb2.SERVICE_CAPABILITY_KIND_FACE_LIVENESS, "lease-liveness"
            ),
            capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION: FakeEvidenceSession(
                capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION, "lease-speaker"
            ),
            capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION: FakeEvidenceSession(
                capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION, "lease-verification"
            ),
        }
        ids = (uuid.UUID(int=value) for value in range(1, 20))
        self.publisher = IdentityEvidencePublisher(
            self.stub,
            self.sessions,
            evidence_ttl=datetime.timedelta(seconds=2),
            utcnow=lambda: self.now,
            uuid_factory=lambda: next(ids),
        )
        self.deadline = self.now + datetime.timedelta(seconds=3)

    def test_publishes_five_strongly_typed_fragments_with_independent_sequences(self):
        candidate = EvidenceCandidate("candidate", "profile-a", 0.8, "model.v1")
        verification = VerificationCandidate("verification", 0.9, "model.v1")

        outcomes = (
            self.publisher.publish_face_detection("window", self.now, self.deadline, 1),
            self.publisher.publish_face_identification("window", self.now, self.deadline, [candidate]),
            self.publisher.publish_face_liveness(
                "window", self.now, self.deadline, identity_pb2.FACE_LIVENESS_STATE_PASSED
            ),
            self.publisher.publish_speaker_identification("window", self.now, self.deadline, [candidate]),
            self.publisher.publish_speaker_verification(
                "challenge", "verification-window", self.now, self.deadline, verification
            ),
        )
        self.assertTrue(all(outcome == PublishDisposition.SUCCEEDED for outcome in outcomes))
        self.assertEqual(len(self.stub.calls), 5)
        self.assertEqual([request.metadata.source_seq for _, request in self.stub.calls], [1] * 5)
        self.assertEqual(
            [request.metadata.provider_lease_id for _, request in self.stub.calls],
            ["lease-detection", "lease-face", "lease-liveness", "lease-speaker", "lease-verification"],
        )
        verification_request = self.stub.calls[-1][1]
        self.assertEqual(verification_request.verification_challenge_id, "challenge")
        self.assertFalse(hasattr(verification_request, "profile_ref"))

    def test_retry_reuses_exact_request_and_sequence_and_is_bounded_per_capability(self):
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION
        self.stub.fail_once = "PublishFaceDetectionEvidence"
        first = self.publisher.publish_face_detection("window", self.now, self.deadline, 1)
        first_wire = self.stub.serialized[-1]
        self.assertEqual(first, PublishDisposition.RETRYABLE)
        with self.assertRaises(RuntimeError):
            self.publisher.publish_face_detection("window-2", self.now, self.deadline, 0)

        second = self.publisher.retry(capability)
        self.assertEqual(second, PublishDisposition.SUCCEEDED)
        self.assertEqual(self.stub.serialized[-1], first_wire)
        self.assertEqual(self.sessions[capability].sequence, 1)
        self.assertFalse(self.publisher.has_pending(capability))

    def test_retry_at_exact_work_deadline_discards_pending(self):
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION
        self.stub.fail_once = "PublishSpeakerIdentificationEvidence"
        result = self.publisher.publish_speaker_identification(
            "window", self.now, self.deadline, []
        )
        self.assertEqual(result, PublishDisposition.RETRYABLE)
        self.now = self.deadline
        self.assertEqual(self.publisher.retry(capability), PublishDisposition.TERMINAL)
        self.assertFalse(self.publisher.has_pending(capability))


def _timestamp(value):
    result = timestamp_pb2.Timestamp()
    result.FromDatetime(value)
    return result


def vision_active(revision, now, encoded):
    return identity_worker_pb2.WatchVisionResponse(
        revision=revision,
        active=identity_worker_pb2.VisionActiveWork(
            evidence_window_id="window",
            opened_at=_timestamp(now - datetime.timedelta(seconds=1)),
            deadline=_timestamp(now + datetime.timedelta(seconds=1)),
            detection=identity_worker_pb2.VisionFaceDetectionTask(),
            identification=identity_worker_pb2.VisionFaceIdentificationTask(
                templates=[identity_worker_pb2.IdentityWorkerIdentificationTemplate(
                    profile_ref="profile-a", encoded_template=encoded
                )]
            ),
            liveness=identity_worker_pb2.VisionFaceLivenessTask(),
        ),
    )


def audio_identification(revision, opened_at, deadline):
    return identity_worker_pb2.WatchAudioResponse(
        revision=revision,
        active=identity_worker_pb2.AudioActiveWork(
            identification=identity_worker_pb2.AudioSpeakerIdentificationWork(
                evidence_window_id="window",
                opened_at=_timestamp(opened_at),
                deadline=_timestamp(deadline),
                templates=[identity_worker_pb2.IdentityWorkerIdentificationTemplate(
                    profile_ref="profile-a", encoded_template=b"speaker"
                )],
            )
        ),
    )


class FakeEvidenceSession:
    def __init__(self, capability, lease_id):
        self.capability = capability
        self.lease_id = lease_id
        self.instance_id = "shared-instance"
        self.rpc_timeout = 1.5
        self.sequence = 0
        self.active = True

    def next_source_sequence(self):
        if not self.active:
            raise RuntimeError("inactive")
        self.sequence += 1
        return self.sequence


class FakeRPCError(grpc.RpcError):
    def code(self):
        return grpc.StatusCode.UNAVAILABLE


class FakeIdentityStub:
    def __init__(self):
        self.calls = []
        self.serialized = []
        self.fail_once = ""

    def __getattr__(self, name):
        if not name.startswith("Publish"):
            raise AttributeError(name)

        def call(request, timeout):
            self.calls.append((name, request))
            self.serialized.append(request.SerializeToString(deterministic=True))
            if self.fail_once == name:
                self.fail_once = ""
                raise FakeRPCError()
            fragment_id = request.metadata.fragment_id
            return _response_for(name, fragment_id)

        return call


def _response_for(method, fragment_id):
    receipt = identity_pb2.IdentityEvidenceReceipt(
        fragment_id=fragment_id,
        status=identity_pb2.IDENTITY_EVIDENCE_RECEIPT_STATUS_ACCEPTED,
        reason=identity_pb2.IDENTITY_EVIDENCE_RECEIPT_REASON_NONE,
    )
    response_type = getattr(identity_pb2, method.replace("Publish", "Publish") + "Response")
    return response_type(receipt=receipt)


if __name__ == "__main__":
    unittest.main()
