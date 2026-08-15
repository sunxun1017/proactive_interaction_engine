import datetime
import threading
import unittest

from google.protobuf import timestamp_pb2
from proactive.platform.v1 import identity_pb2, identity_worker_pb2

from workers.identity_control import EvidenceCandidate, LatestIdentityWork
from workers.vision.stack import VisionInferenceResult, VisionStack


UTC = datetime.timezone.utc


class VisionStackTest(unittest.TestCase):
    def setUp(self):
        self.now = datetime.datetime(2026, 8, 15, 12, 0, tzinfo=UTC)
        self.latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        self.latest.begin_stream()
        self.latest.apply(vision_work(1, self.now))
        self.source = FakeFrameSource([bytearray(b"first"), bytearray(b"second")])
        self.base = FakeBaseSession()
        self.publisher = FakeVisionPublisher()
        self.inference = FakeVisionInference()
        self.stack = VisionStack(
            self.source,
            lambda frame: bool(frame),
            ImmediatePresenceGate(),
            self.base,
            (),
            self.latest,
            self.publisher,
            self.inference,
            utcnow=lambda: self.now,
            clone_frame=lambda frame: bytearray(frame),
            join_timeout=1.0,
        )

    def test_base_runs_for_every_frame_while_identity_uses_capacity_one_latest_frame(self):
        self.assertTrue(self.stack.capture_once())
        self.assertTrue(self.stack.capture_once())
        self.assertEqual(self.base.maintained, 2)
        self.assertEqual(self.base.presence, [True])

        self.assertTrue(self.stack.process_identity_once())
        self.assertEqual(self.inference.frames, [b"second"])
        self.assertEqual([kind for kind, _ in self.publisher.calls], ["detection", "identification", "liveness"])
        self.assertEqual(self.stack.buffered_frames, 0)

    def test_old_revision_and_exact_deadline_drop_results(self):
        self.assertTrue(self.stack.capture_once())
        self.inference.on_infer = lambda: self.latest.apply(identity_worker_pb2.WatchVisionResponse(
            revision=2, idle=identity_worker_pb2.IdentityWorkerIdle()
        ))
        self.assertFalse(self.stack.process_identity_once())
        self.assertEqual(self.publisher.calls, [])

        self.latest.apply(vision_work(3, self.now))
        self.assertTrue(self.stack.capture_once())
        self.now += datetime.timedelta(seconds=1)
        self.assertFalse(self.stack.process_identity_once())
        self.assertEqual(self.publisher.calls, [])

    def test_run_closes_source_sessions_and_joins_owned_thread(self):
        latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        latest.begin_stream()
        source = FakeFrameSource([])
        base = FakeBaseSession()
        identity_session = FakeBaseSession()
        stack = VisionStack(
            source,
            lambda _frame: False,
            ImmediatePresenceGate(),
            base,
            (identity_session,),
            latest,
            FakeVisionPublisher(),
            FakeVisionInference(),
            utcnow=lambda: self.now,
            clone_frame=lambda frame: bytearray(frame),
            join_timeout=1.0,
        )
        stack.run(threading.Event())
        self.assertTrue(source.closed)
        self.assertEqual(base.closed, 1)
        self.assertEqual(identity_session.closed, 1)
        self.assertEqual(stack.owned_threads_alive, 0)

    def test_shutdown_waits_for_executor_before_wiping_claimed_input(self):
        latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(vision_work(1, self.now))
        source = BlockingFrameSource(bytearray(b"blocking-frame"))
        publisher = FakeVisionPublisher()
        inference = BlockingVisionInference()
        stack = VisionStack(
            source,
            lambda _frame: False,
            ImmediatePresenceGate(),
            FakeBaseSession(),
            (),
            latest,
            publisher,
            inference,
            utcnow=lambda: self.now,
            clone_frame=lambda frame: bytearray(frame),
            join_timeout=1.0,
        )
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
        self.assertEqual(bytes(inference.template_bytes), b"face-template")
        self.assertEqual(bytes(inference.frame), b"blocking-frame")

        inference.release.set()
        close_thread.join(1)
        run_thread.join(1)
        self.assertEqual(close_errors, [])
        self.assertEqual(run_errors, [])
        self.assertEqual(bytes(inference.template_bytes), bytes(len(b"face-template")))
        self.assertEqual(bytes(inference.frame), bytes(len(b"blocking-frame")))
        self.assertEqual(publisher.calls, [])

    def test_shutdown_timeout_fails_without_wiping_live_executor_input(self):
        latest = LatestIdentityWork("vision", utcnow=lambda: self.now)
        latest.begin_stream()
        latest.apply(vision_work(1, self.now))
        source = BlockingFrameSource(bytearray(b"timeout-frame"))
        inference = BlockingVisionInference()
        stack = VisionStack(
            source,
            lambda _frame: False,
            ImmediatePresenceGate(),
            FakeBaseSession(),
            (),
            latest,
            FakeVisionPublisher(),
            inference,
            utcnow=lambda: self.now,
            clone_frame=lambda frame: bytearray(frame),
            join_timeout=0.02,
        )
        run_errors = []
        run_thread = threading.Thread(target=lambda: _capture_error(run_errors, stack.run, threading.Event()))
        run_thread.start()
        self.assertTrue(inference.entered.wait(1))

        with self.assertRaisesRegex(RuntimeError, "failed to join"):
            stack.close()
        self.assertEqual(bytes(inference.template_bytes), b"face-template")
        self.assertEqual(bytes(inference.frame), b"timeout-frame")

        inference.release.set()
        run_thread.join(1)
        stack.close()
        self.assertEqual(bytes(inference.template_bytes), bytes(len(b"face-template")))


def _timestamp(value):
    result = timestamp_pb2.Timestamp()
    result.FromDatetime(value)
    return result


def vision_work(revision, now):
    return identity_worker_pb2.WatchVisionResponse(
        revision=revision,
        active=identity_worker_pb2.VisionActiveWork(
            evidence_window_id="window",
            opened_at=_timestamp(now - datetime.timedelta(seconds=1)),
            deadline=_timestamp(now + datetime.timedelta(seconds=1)),
            detection=identity_worker_pb2.VisionFaceDetectionTask(),
            identification=identity_worker_pb2.VisionFaceIdentificationTask(
                templates=[identity_worker_pb2.IdentityWorkerIdentificationTemplate(
                    profile_ref="profile-a", encoded_template=b"face-template"
                )]
            ),
            liveness=identity_worker_pb2.VisionFaceLivenessTask(),
        ),
    )


class FakeFrameSource:
    def __init__(self, frames):
        self.frames = list(frames)
        self.closed = False

    def read(self):
        if not self.frames:
            return False, None
        return True, self.frames.pop(0)

    def close(self):
        self.closed = True


class BlockingFrameSource:
    def __init__(self, frame):
        self._frame = frame
        self._first = True
        self.closed_event = threading.Event()

    def read(self):
        if self._first:
            self._first = False
            return True, self._frame
        self.closed_event.wait(2)
        return False, None

    def close(self):
        self.closed_event.set()


class ImmediatePresenceGate:
    def __init__(self):
        self.sent = False

    def update(self, present):
        if present and not self.sent:
            self.sent = True
            return True
        return None


class FakeBaseSession:
    def __init__(self):
        self.maintained = 0
        self.presence = []
        self.closed = 0

    def maintain(self):
        self.maintained += 1
        return True

    def publish_presence(self, present):
        self.presence.append(present)
        return True

    def close(self):
        self.closed += 1


class FakeVisionInference:
    def __init__(self):
        self.frames = []
        self.on_infer = None

    def infer(self, work, frame):
        self.frames.append(bytes(frame))
        if self.on_infer is not None:
            self.on_infer()
        return VisionInferenceResult(
            faces_observed=1,
            candidates=(EvidenceCandidate("face", "profile-a", 0.9, "face.v1"),),
            liveness=identity_pb2.FACE_LIVENESS_STATE_PASSED,
        )


class BlockingVisionInference:
    def __init__(self):
        self.entered = threading.Event()
        self.release = threading.Event()
        self.template_bytes = None
        self.frame = None

    def infer(self, work, frame):
        self.template_bytes = work.identification_templates[0].encoded
        self.frame = frame
        self.entered.set()
        self.release.wait(2)
        return VisionInferenceResult(1)


class FakeVisionPublisher:
    def __init__(self):
        self.calls = []

    def publish_face_detection(self, *args):
        self.calls.append(("detection", args))

    def publish_face_identification(self, *args):
        self.calls.append(("identification", args))

    def publish_face_liveness(self, *args):
        self.calls.append(("liveness", args))

    def discard(self, _capability):
        pass


def _capture_error(errors, function, *args):
    try:
        function(*args)
    except Exception as error:  # Test records the exact shutdown result across a thread.
        errors.append(error)


if __name__ == "__main__":
    unittest.main()
