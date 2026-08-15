import math
import unittest

from workers.speaker.model import SpeakerModel, affine_cosine_score, normalize_embedding
from workers.speaker.template import EMBEDDING_DIMENSION, decode_template


MODEL_SHA256 = "cd" * 32


def basis(index, scale=1.0):
    values = [0.0] * EMBEDDING_DIMENSION
    values[index] = scale
    return tuple(values)


class SpeakerModelTest(unittest.TestCase):
    def test_builds_normalized_centroid_template_from_injected_backend(self):
        backend = FakeBackend([basis(0), basis(1)])
        model = SpeakerModel(backend)

        payload = model.enroll((b"\x00\x00" * 16_000, b"\x01\x00" * 16_000))

        decoded = decode_template(payload)
        self.assertEqual(decoded.model_sha256, MODEL_SHA256)
        self.assertEqual(decoded.enrollment_count, 2)
        self.assertAlmostEqual(decoded.embedding[0], 1 / math.sqrt(2), places=6)
        self.assertAlmostEqual(decoded.embedding[1], 1 / math.sqrt(2), places=6)
        self.assertEqual(backend.sample_rates, [16_000, 16_000])

    def test_scores_cosine_with_exact_monotonic_range_mapping(self):
        enrolled = SpeakerModel(FakeBackend([basis(0)])).enroll((b"\x00\x00",))

        cases = ((basis(0), 1.0), (basis(0, -1.0), 0.0), (basis(1), 0.5))
        for candidate, expected in cases:
            with self.subTest(expected=expected):
                model = SpeakerModel(FakeBackend([candidate]))
                self.assertAlmostEqual(model.score(b"\x00\x00", enrolled), expected)

    def test_score_rejects_template_from_another_model(self):
        enrolled = SpeakerModel(FakeBackend([basis(0)], model_sha256="ef" * 32)).enroll((b"\x00\x00",))

        with self.assertRaises(ValueError):
            SpeakerModel(FakeBackend([basis(0)])).score(b"\x00\x00", enrolled)

    def test_rejects_invalid_pcm_and_enrollment_bounds_before_backend(self):
        model = SpeakerModel(FakeBackend([basis(0)] * 20))
        for pcm in (b"", b"\x00", "not bytes"):
            with self.subTest(pcm=pcm):
                with self.assertRaises(ValueError):
                    model.extract(pcm)
        with self.assertRaises(ValueError):
            model.extract(b"\x00\x00", sample_rate=8_000)
        with self.assertRaises(ValueError):
            model.enroll(())
        with self.assertRaises(ValueError):
            model.enroll((b"\x00\x00",) * 17)

    def test_rejects_bad_backend_embedding(self):
        invalid = (
            (),
            (0.0,) * EMBEDDING_DIMENSION,
            (1.0,) * (EMBEDDING_DIMENSION - 1) + (math.inf,),
        )
        for embedding in invalid:
            with self.subTest(length=len(embedding)):
                with self.assertRaises(ValueError):
                    SpeakerModel(FakeBackend([embedding])).extract(b"\x00\x00")


class ScoreMappingTest(unittest.TestCase):
    def test_normalizes_without_accepting_invalid_values(self):
        normalized = normalize_embedding((2.0,) * EMBEDDING_DIMENSION)

        self.assertAlmostEqual(math.sqrt(sum(value * value for value in normalized)), 1.0)

    def test_affine_mapping_is_bounded_without_threshold(self):
        self.assertEqual(affine_cosine_score(-1.0), 0.0)
        self.assertEqual(affine_cosine_score(0.0), 0.5)
        self.assertEqual(affine_cosine_score(1.0), 1.0)
        with self.assertRaises(ValueError):
            affine_cosine_score(math.nan)
        with self.assertRaises(ValueError):
            affine_cosine_score(1.001)


class FakeBackend:
    def __init__(self, embeddings, model_sha256=MODEL_SHA256):
        self.model_sha256 = model_sha256
        self._embeddings = iter(embeddings)
        self.sample_rates = []

    def embed(self, pcm_s16le, sample_rate):
        self.sample_rates.append(sample_rate)
        return next(self._embeddings)


if __name__ == "__main__":
    unittest.main()
