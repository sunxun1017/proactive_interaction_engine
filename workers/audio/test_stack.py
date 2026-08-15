import datetime
import threading
import unittest

from google.protobuf import timestamp_pb2
from proactive.platform.v1 import identity_worker_pb2

from workers.audio.stack import AudioInferenceResult, AudioStack
from workers.identity_control import (
    EvidenceCandidate,
    LatestIdentityWork,
    VerificationCandidate,
)


UTC = datetime.timezone.utc


class AudioStackTest(unittest.TestCase):
    def setUp(self):
        self.now = datetime.datetime(2026, 8, 15, 13, 0, tzinfo=UTC)
        self.latest = LatestIdentityWork("audio", utcnow=lambda: self.now)
        self.latest.begin_stream()
        self.latest.apply(audio_identification(1, self.now))
        self.source = FakeAudioSource([b"a" * 640, b"b" * 640, b"c" * 640])
        self.base = FakeVADSession()
        self.publisher = FakeAudioPublisher()
        self.inference = FakeAudioInference()
        self.stack = AudioStack(
            self.source,
            AlwaysVoice(),
            ImmediateVoiceGate(),
            self.base,
            (),
            self.latest,
            self.publisher,
            self.inference,
            max_pcm_bytes=1280,
            segment_ready=lambda buffered: buffered >= 1280,
            utcnow=lambda: self.now,
            join_timeout=1.0,
        )

    def test_vad_continues_and_identity_uses_bounded_latest_pcm(self):
        self.assertFalse(self.stack.process_identity_once())
        for _ in range(3):
            self.assertTrue(self.stack.capture_once())
        self.assertEqual(self.base.maintained, 3)
        self.assertEqual(self.base.activities, 1)
        self.assertEqual(self.stack.buffered_pcm_bytes, 1280)

        self.assertTrue(self.stack.process_identity_once())
        self.assertEqual(self.inference.pcm, [b"b" * 640 + b"c" * 640])
        self.assertEqual([kind for kind, _ in self.publisher.calls], ["identification"])
        self.assertEqual(self.stack.buffered_pcm_bytes, 0)

    def test_verification_is_strict_and_never_passes_expected_profile(self):
        latest = LatestIdentityWork("audio", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(audio_verification(1, self.now))
        source = FakeAudioSource([b"v" * 640])
        publisher = FakeAudioPublisher()
        inference = FakeAudioInference()
        stack = AudioStack(
            source,
            AlwaysVoice(),
            ImmediateVoiceGate(),
            FakeVADSession(),
            (),
            latest,
            publisher,
            inference,
            max_pcm_bytes=640,
            segment_ready=lambda buffered: buffered == 640,
            utcnow=lambda: self.now,
            join_timeout=1.0,
        )
        self.assertFalse(stack.process_identity_once())
        self.assertTrue(stack.capture_once())
        self.assertTrue(stack.process_identity_once())
        kind, args = publisher.calls[0]
        self.assertEqual(kind, "verification")
        self.assertEqual(args[0:2], ("challenge", "verification-window"))
        self.assertNotIn("profile-a", repr(args))

    def test_old_revision_and_exact_deadline_discard_audio(self):
        self.assertFalse(self.stack.process_identity_once())
        self.assertTrue(self.stack.capture_once())
        self.assertTrue(self.stack.capture_once())
        self.inference.on_infer = lambda: self.latest.apply(identity_worker_pb2.WatchAudioResponse(
            revision=2, idle=identity_worker_pb2.IdentityWorkerIdle()
        ))
        self.assertFalse(self.stack.process_identity_once())
        self.assertEqual(self.publisher.calls, [])

        self.latest.apply(audio_identification(3, self.now))
        self.assertFalse(self.stack.process_identity_once())
        self.assertTrue(self.stack.capture_once())
        self.now += datetime.timedelta(seconds=1)
        self.assertFalse(self.stack.process_identity_once())
        self.assertEqual(self.publisher.calls, [])

    def test_run_closes_pcm_source_sessions_and_joins_owned_thread(self):
        latest = LatestIdentityWork("audio", utcnow=lambda: self.now)
        latest.begin_stream()
        source = FakeAudioSource([])
        base = FakeVADSession()
        identity_session = FakeVADSession()
        stack = AudioStack(
            source,
            AlwaysVoice(),
            ImmediateVoiceGate(),
            base,
            (identity_session,),
            latest,
            FakeAudioPublisher(),
            FakeAudioInference(),
            max_pcm_bytes=640,
            segment_ready=lambda _buffered: False,
            utcnow=lambda: self.now,
            join_timeout=1.0,
        )
        stack.run(threading.Event())
        self.assertTrue(source.closed)
        self.assertEqual(base.closed, 1)
        self.assertEqual(identity_session.closed, 1)
        self.assertEqual(stack.owned_threads_alive, 0)

    def test_shutdown_waits_for_executor_before_wiping_pcm_and_expected_template(self):
        latest = LatestIdentityWork("audio", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(audio_verification(1, self.now))
        source = BlockingAudioSource(b"z" * 640)
        publisher = FakeAudioPublisher()
        inference = BlockingAudioInference()
        stack = AudioStack(
            source,
            AlwaysVoice(),
            ImmediateVoiceGate(),
            FakeVADSession(),
            (),
            latest,
            publisher,
            inference,
            max_pcm_bytes=640,
            segment_ready=lambda buffered: buffered == 640,
            utcnow=lambda: self.now,
            join_timeout=1.0,
        )
        self.assertFalse(stack.process_identity_once())
        run_errors = []
        run_thread = threading.Thread(target=lambda: _capture_error(run_errors, stack.run, threading.Event()))
        run_thread.start()
        self.assertTrue(inference.entered.wait(1))

        close_errors = []
        close_done = threading.Event()
        close_thread = threading.Thread(
            target=lambda: (_capture_error(close_errors, stack.close), close_done.set())
        )
        close_thread.start()
        self.assertTrue(source.closed_event.wait(1))
        self.assertFalse(close_done.is_set())
        self.assertEqual(bytes(inference.expected), b"expected-speaker-template")
        self.assertEqual(stack.buffered_pcm_bytes, 640)

        inference.release.set()
        close_thread.join(1)
        run_thread.join(1)
        self.assertEqual(close_errors, [])
        self.assertEqual(run_errors, [])
        self.assertEqual(bytes(inference.expected), bytes(len(b"expected-speaker-template")))
        self.assertEqual(stack.buffered_pcm_bytes, 0)
        self.assertEqual(publisher.calls, [])

    def test_shutdown_timeout_fails_without_wiping_live_audio_input(self):
        latest = LatestIdentityWork("audio", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(audio_verification(1, self.now))
        source = BlockingAudioSource(b"q" * 640)
        inference = BlockingAudioInference()
        stack = AudioStack(
            source,
            AlwaysVoice(),
            ImmediateVoiceGate(),
            FakeVADSession(),
            (),
            latest,
            FakeAudioPublisher(),
            inference,
            max_pcm_bytes=640,
            segment_ready=lambda buffered: buffered == 640,
            utcnow=lambda: self.now,
            join_timeout=0.02,
        )
        self.assertFalse(stack.process_identity_once())
        run_errors = []
        run_thread = threading.Thread(target=lambda: _capture_error(run_errors, stack.run, threading.Event()))
        run_thread.start()
        self.assertTrue(inference.entered.wait(1))

        with self.assertRaisesRegex(RuntimeError, "failed to join"):
            stack.close()
        self.assertEqual(bytes(inference.expected), b"expected-speaker-template")
        self.assertEqual(stack.buffered_pcm_bytes, 640)

        inference.release.set()
        run_thread.join(1)
        stack.close()
        self.assertEqual(bytes(inference.expected), bytes(len(b"expected-speaker-template")))


def _timestamp(value):
    result = timestamp_pb2.Timestamp()
    result.FromDatetime(value)
    return result


def audio_identification(revision, now):
    return identity_worker_pb2.WatchAudioResponse(
        revision=revision,
        active=identity_worker_pb2.AudioActiveWork(
            identification=identity_worker_pb2.AudioSpeakerIdentificationWork(
                evidence_window_id="audio-window",
                opened_at=_timestamp(now - datetime.timedelta(seconds=1)),
                deadline=_timestamp(now + datetime.timedelta(seconds=1)),
                templates=[identity_worker_pb2.IdentityWorkerIdentificationTemplate(
                    profile_ref="profile-a", encoded_template=b"speaker-template"
                )],
            )
        ),
    )


def audio_verification(revision, now):
    return identity_worker_pb2.WatchAudioResponse(
        revision=revision,
        active=identity_worker_pb2.AudioActiveWork(
            verification=identity_worker_pb2.AudioSpeakerVerificationWork(
                verification_challenge_id="challenge",
                evidence_window_id="verification-window",
                opened_at=_timestamp(now - datetime.timedelta(seconds=1)),
                deadline=_timestamp(now + datetime.timedelta(seconds=1)),
                expected_encoded_template=b"expected-speaker-template",
            )
        ),
    )


class FakeAudioSource:
    def __init__(self, chunks):
        self.chunks = list(chunks)
        self.closed = False

    def read(self, size):
        if not self.chunks:
            return b""
        chunk = self.chunks.pop(0)
        if len(chunk) != size:
            raise AssertionError("unexpected requested PCM frame size")
        return chunk

    def close(self):
        self.closed = True


class BlockingAudioSource:
    def __init__(self, chunk):
        self._chunk = chunk
        self._first = True
        self.closed_event = threading.Event()

    def read(self, size):
        if self._first:
            self._first = False
            if len(self._chunk) != size:
                raise AssertionError("unexpected requested PCM frame size")
            return self._chunk
        self.closed_event.wait(2)
        return b""

    def close(self):
        self.closed_event.set()


class AlwaysVoice:
    def is_speech(self, _frame, _sample_rate):
        return True


class ImmediateVoiceGate:
    def __init__(self):
        self.sent = False

    def update(self, voiced):
        if voiced and not self.sent:
            self.sent = True
            return True
        return False


class FakeVADSession:
    def __init__(self):
        self.maintained = 0
        self.activities = 0
        self.closed = 0

    def maintain(self):
        self.maintained += 1
        return True

    def publish_speech_activity(self):
        self.activities += 1
        return True

    def close(self):
        self.closed += 1


class FakeAudioInference:
    def __init__(self):
        self.pcm = []
        self.on_infer = None

    def infer(self, work, pcm):
        self.pcm.append(bytes(pcm))
        if self.on_infer is not None:
            self.on_infer()
        if work.kind == "verification":
            return AudioInferenceResult(
                verification=VerificationCandidate("candidate", 0.9, "speaker.v1")
            )
        return AudioInferenceResult(
            candidates=(EvidenceCandidate("candidate", "profile-a", 0.8, "speaker.v1"),)
        )


class BlockingAudioInference:
    def __init__(self):
        self.entered = threading.Event()
        self.release = threading.Event()
        self.expected = None

    def infer(self, work, _pcm):
        self.expected = work.expected_encoded
        self.entered.set()
        self.release.wait(2)
        return AudioInferenceResult(
            verification=VerificationCandidate("candidate", 0.9, "speaker.v1")
        )


class FakeAudioPublisher:
    def __init__(self):
        self.calls = []

    def publish_speaker_identification(self, *args):
        self.calls.append(("identification", args))

    def publish_speaker_verification(self, *args):
        self.calls.append(("verification", args))

    def discard(self, _capability):
        pass


def _capture_error(errors, function, *args):
    try:
        function(*args)
    except Exception as error:  # Test records the exact shutdown result across a thread.
        errors.append(error)


if __name__ == "__main__":
    unittest.main()
