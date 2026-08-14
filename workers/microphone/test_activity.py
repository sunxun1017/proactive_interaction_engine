import unittest

from workers.microphone.activity import (
    MicrophoneWorker,
    PCMFramer,
    VoiceActivityGate,
    parec_command,
)


class PCMFramerTest(unittest.TestCase):
    def test_yields_exact_frames_and_retains_only_incomplete_tail(self):
        framer = PCMFramer(sample_rate=16_000, frame_ms=20, sample_width=2)
        payload = bytes(range(256)) * 6

        frames = framer.push(payload)

        self.assertEqual([len(frame) for frame in frames], [640, 640])
        self.assertLess(framer.buffered_bytes, 640)
        self.assertEqual(b"".join(frames) + framer.tail, payload)

    def test_rejects_unsupported_frame_shape(self):
        for rate, frame_ms, width in ((0, 30, 2), (16_000, 25, 2), (16_000, 30, 1)):
            with self.subTest(rate=rate, frame_ms=frame_ms, width=width):
                with self.assertRaises(ValueError):
                    PCMFramer(rate, frame_ms, width)


class VoiceActivityGateTest(unittest.TestCase):
    def test_requires_twelve_of_fifteen_frames_then_waits_for_silence(self):
        gate = VoiceActivityGate(window_frames=15, voiced_frames=12, rearm_silence_frames=25)

        sequence = [True] * 11 + [False] * 3
        for voiced in sequence:
            self.assertFalse(gate.update(voiced))
        self.assertTrue(gate.update(True))
        for _ in range(30):
            self.assertFalse(gate.update(True))
        for _ in range(24):
            self.assertFalse(gate.update(False))
        self.assertFalse(gate.update(False))
        for _ in range(14):
            self.assertFalse(gate.update(True))
        self.assertTrue(gate.update(True))

    def test_window_tolerates_three_unvoiced_frames(self):
        gate = VoiceActivityGate(window_frames=5, voiced_frames=3, rearm_silence_frames=2)

        self.assertFalse(gate.update(True))
        self.assertFalse(gate.update(False))
        self.assertFalse(gate.update(True))
        self.assertFalse(gate.update(False))
        self.assertTrue(gate.update(True))

    def test_rejects_invalid_threshold(self):
        for values in ((0, 1, 1), (3, 0, 1), (3, 4, 1), (3, 1, 0)):
            with self.subTest(values=values):
                with self.assertRaises(ValueError):
                    VoiceActivityGate(*values)


class MicrophoneWorkerTest(unittest.TestCase):
    def test_publishes_one_activity_after_stable_voice_and_retains_no_frames(self):
        vad = SequenceVAD([True] * 12 + [False] * 3)
        session = FakeSession()
        worker = MicrophoneWorker(vad, session)

        worker.process(bytes(640 * 15))

        self.assertEqual(session.activities, 1)
        self.assertEqual(worker.buffered_bytes, 0)

    def test_parec_command_is_fixed_mono_s16le_16khz_without_shell(self):
        self.assertEqual(
            parec_command("/usr/bin/parec"),
            [
                "/usr/bin/parec",
                "--raw",
                "--format=s16le",
                "--rate=16000",
                "--channels=1",
            ],
        )


class SequenceVAD:
    def __init__(self, values):
        self._values = iter(values)

    def is_speech(self, frame, sample_rate):
        self.last_shape = (len(frame), sample_rate)
        return next(self._values)


class FakeSession:
    def __init__(self):
        self.activities = 0

    def maintain(self):
        return True

    def publish_speech_activity(self):
        self.activities += 1
        return True


if __name__ == "__main__":
    unittest.main()
