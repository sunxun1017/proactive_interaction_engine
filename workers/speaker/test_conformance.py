import hashlib
import pathlib
import tempfile
import unittest
import wave

from workers.speaker.conformance import (
    latency_summary,
    read_s16le_mono_16k_wav,
    verify_conformance_asset,
)


class ConformanceInputTest(unittest.TestCase):
    def test_reads_only_exact_16khz_mono_s16le_wav(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "sample.wav"
            with wave.open(str(path), "wb") as output:
                output.setnchannels(1)
                output.setsampwidth(2)
                output.setframerate(16_000)
                output.writeframes(b"\x01\x00" * 800)

            self.assertEqual(read_s16le_mono_16k_wav(path), b"\x01\x00" * 800)

    def test_rejects_incompatible_or_empty_wav(self):
        with tempfile.TemporaryDirectory() as directory:
            for name, channels, width, rate, frames in (
                ("stereo.wav", 2, 2, 16_000, b"\x00" * 8),
                ("u8.wav", 1, 1, 16_000, b"\x00" * 8),
                ("8k.wav", 1, 2, 8_000, b"\x00" * 8),
                ("empty.wav", 1, 2, 16_000, b""),
            ):
                path = pathlib.Path(directory) / name
                with wave.open(str(path), "wb") as output:
                    output.setnchannels(channels)
                    output.setsampwidth(width)
                    output.setframerate(rate)
                    output.writeframes(frames)
                with self.subTest(name=name):
                    with self.assertRaises(ValueError):
                        read_s16le_mono_16k_wav(path)

    def test_reports_bounded_nearest_rank_latency_quantiles(self):
        self.assertEqual(
            latency_summary([5.0, 1.0, 4.0, 2.0, 3.0]),
            {"count": 5, "min_ms": 1.0, "p50_ms": 3.0, "p95_ms": 5.0, "p99_ms": 5.0, "max_ms": 5.0},
        )
        with self.assertRaises(ValueError):
            latency_summary([])

    def test_conformance_asset_requires_exact_regular_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "sample.wav"
            payload = b"official example bytes"
            path.write_bytes(payload)
            digest = hashlib.sha256(payload).hexdigest()

            verify_conformance_asset(path, len(payload), digest)
            with self.assertRaises(ValueError):
                verify_conformance_asset(path, len(payload) + 1, digest)
            with self.assertRaises(ValueError):
                verify_conformance_asset(path, len(payload), "00" * 32)


if __name__ == "__main__":
    unittest.main()
