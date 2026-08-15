import math
import struct
import unittest

import numpy as np

from workers.vision.template import (
    FACE_TEMPLATE_DIMENSION,
    FACE_TEMPLATE_MODEL_SHA256,
    FaceTemplate,
    aggregate_embeddings,
    decode_face_template,
    encode_face_template,
    normalize_embedding,
)


class FaceTemplateTest(unittest.TestCase):
    def test_round_trips_fixed_finite_unit_embedding(self):
        embedding = np.zeros(FACE_TEMPLATE_DIMENSION, dtype=np.float32)
        embedding[7] = 1.0
        encoded = encode_face_template(FaceTemplate(embedding=embedding, sample_count=5))

        decoded = decode_face_template(encoded)

        self.assertEqual(decoded.sample_count, 5)
        self.assertEqual(decoded.embedding.dtype, np.float32)
        np.testing.assert_array_equal(decoded.embedding, embedding)
        self.assertEqual(len(encoded), 50 + FACE_TEMPLATE_DIMENSION * 4)

    def test_encoding_rejects_wrong_dimension_nonfinite_nonunit_and_sample_count(self):
        valid = np.zeros(FACE_TEMPLATE_DIMENSION, dtype=np.float32)
        valid[0] = 1.0
        cases = (
            FaceTemplate(np.ones(FACE_TEMPLATE_DIMENSION - 1), 1),
            FaceTemplate(np.full(FACE_TEMPLATE_DIMENSION, math.nan), 1),
            FaceTemplate(np.ones(FACE_TEMPLATE_DIMENSION), 1),
            FaceTemplate(valid, 0),
            FaceTemplate(valid, 65536),
        )
        for template in cases:
            with self.subTest(template=template), self.assertRaises(ValueError):
                encode_face_template(template)

    def test_decoding_rejects_wrong_magic_version_model_dimension_and_trailing_bytes(self):
        embedding = np.zeros(FACE_TEMPLATE_DIMENSION, dtype=np.float32)
        embedding[0] = 1.0
        encoded = bytearray(encode_face_template(FaceTemplate(embedding, 1)))
        cases = []
        wrong_magic = bytearray(encoded)
        wrong_magic[:8] = b"NOTFACE!"
        cases.append(wrong_magic)
        wrong_version = bytearray(encoded)
        struct.pack_into("<H", wrong_version, 8, 2)
        cases.append(wrong_version)
        wrong_profile = bytearray(encoded)
        struct.pack_into("<H", wrong_profile, 10, 2)
        cases.append(wrong_profile)
        wrong_dimension = bytearray(encoded)
        struct.pack_into("<H", wrong_dimension, 12, FACE_TEMPLATE_DIMENSION - 1)
        cases.append(wrong_dimension)
        wrong_model = bytearray(encoded)
        wrong_model[18] ^= 0xFF
        cases.append(wrong_model)
        cases.append(encoded + b"trailing")

        for payload in cases:
            with self.subTest(payload=payload[:16]), self.assertRaises(ValueError):
                decode_face_template(bytes(payload))

        self.assertEqual(len(bytes.fromhex(FACE_TEMPLATE_MODEL_SHA256)), 32)

    def test_normalizes_and_aggregates_each_sample_before_mean(self):
        first = np.zeros(FACE_TEMPLATE_DIMENSION)
        second = np.zeros(FACE_TEMPLATE_DIMENSION)
        first[0] = 2.0
        second[1] = 4.0

        normalized = normalize_embedding(first)
        aggregated = aggregate_embeddings((first, second))

        self.assertAlmostEqual(float(np.linalg.norm(normalized)), 1.0, places=6)
        self.assertEqual(aggregated.sample_count, 2)
        self.assertAlmostEqual(float(aggregated.embedding[0]), 2**-0.5, places=6)
        self.assertAlmostEqual(float(aggregated.embedding[1]), 2**-0.5, places=6)

    def test_normalization_rejects_zero_and_nonfinite_vectors(self):
        for embedding in (
            np.zeros(FACE_TEMPLATE_DIMENSION),
            np.full(FACE_TEMPLATE_DIMENSION, np.inf),
        ):
            with self.subTest(), self.assertRaises(ValueError):
                normalize_embedding(embedding)
