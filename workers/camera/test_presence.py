import unittest

from workers.camera.presence import AnonymousPeopleDetector, CameraWorker, PresenceHysteresis


class PresenceHysteresisTest(unittest.TestCase):
    def test_enters_on_three_of_five_and_exits_after_ten_consecutive_misses(self):
        gate = PresenceHysteresis(window_frames=5, present_hits=3, absent_frames=10)

        for detected in (True, False, True, False):
            self.assertIsNone(gate.update(detected))
        self.assertTrue(gate.update(True))

        for _ in range(9):
            self.assertIsNone(gate.update(False))
        self.assertFalse(gate.update(False))
        self.assertIsNone(gate.update(False))

    def test_positive_frame_breaks_absence_run(self):
        gate = PresenceHysteresis(window_frames=3, present_hits=2, absent_frames=3)

        self.assertIsNone(gate.update(True))
        self.assertTrue(gate.update(True))
        self.assertIsNone(gate.update(False))
        self.assertIsNone(gate.update(True))
        self.assertIsNone(gate.update(False))
        self.assertIsNone(gate.update(False))
        self.assertFalse(gate.update(False))

    def test_rejects_invalid_thresholds(self):
        for window, hits, absent in ((0, 1, 1), (3, 0, 1), (3, 4, 1), (3, 1, 0)):
            with self.subTest(window=window, hits=hits, absent=absent):
                with self.assertRaises(ValueError):
                    PresenceHysteresis(window, hits, absent)


class CameraWorkerTest(unittest.TestCase):
    def test_publishes_only_stable_presence_transitions(self):
        source = FakeFrameSource([(True, object())] * 15)
        detector = SequenceDetector([True, False, True, False, True] + [False] * 10)
        session = FakeSession()
        worker = CameraWorker(source, detector, session)

        for _ in range(15):
            self.assertTrue(worker.step())

        self.assertEqual(session.presence, [True, False])
        self.assertEqual(session.maintained, 15)

    def test_three_read_failures_report_unhealthy_without_fake_absence(self):
        source = FakeFrameSource([(False, None)] * 3)
        session = FakeSession()
        worker = CameraWorker(source, SequenceDetector([]), session)

        self.assertTrue(worker.step())
        self.assertTrue(worker.step())
        self.assertFalse(worker.step())

        self.assertEqual(session.presence, [])
        self.assertEqual(session.unhealthy, ["DEVICE_UNAVAILABLE"])

    def test_retries_rejected_transition_after_scenario_becomes_ready(self):
        source = FakeFrameSource([(True, object())] * 6)
        session = FakeSession(publish_results=[False, False, True])
        worker = CameraWorker(source, SequenceDetector([True] * 6), session)

        for _ in range(6):
            self.assertTrue(worker.step())

        self.assertEqual(session.presence, [True, True, True])

    def test_anonymous_detector_uses_no_face_cascade(self):
        cv2 = FakeCV2()

        AnonymousPeopleDetector(cv2)

        self.assertEqual(cv2.cascade_path, "/models/haarcascade_upperbody.xml")
        self.assertNotIn("face", cv2.cascade_path)


class FakeFrameSource:
    def __init__(self, reads):
        self._reads = iter(reads)

    def read(self):
        return next(self._reads)


class SequenceDetector:
    def __init__(self, results):
        self._results = iter(results)

    def __call__(self, _frame):
        return next(self._results)


class FakeSession:
    def __init__(self, publish_results=None):
        self.maintained = 0
        self.presence = []
        self.unhealthy = []
        self._publish_results = iter(publish_results or [])

    def maintain(self):
        self.maintained += 1
        return True

    def publish_presence(self, present):
        self.presence.append(present)
        return next(self._publish_results, True)

    def mark_unhealthy(self, reason):
        self.unhealthy.append(reason)


class FakeHOG:
    def setSVMDetector(self, _detector):
        pass


class FakeCascade:
    def empty(self):
        return False


class FakeCV2:
    class data:
        haarcascades = "/models/"

    def HOGDescriptor(self):
        return FakeHOG()

    def HOGDescriptor_getDefaultPeopleDetector(self):
        return object()

    def CascadeClassifier(self, path):
        self.cascade_path = path
        return FakeCascade()


if __name__ == "__main__":
    unittest.main()
