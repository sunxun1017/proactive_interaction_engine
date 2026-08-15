import math
import struct
import unittest

from workers.speaker.template import (
    EMBEDDING_DIMENSION,
    TEMPLATE_BYTES,
    SpeakerTemplate,
    decode_template,
    encode_template,
)


MODEL_SHA256 = "ab" * 32


def unit_embedding(index=0):
    values = [0.0] * EMBEDDING_DIMENSION
    values[index] = 1.0
    return tuple(values)


class SpeakerTemplateCodecTest(unittest.TestCase):
    def test_round_trips_canonical_fixed_width_template(self):
        original = SpeakerTemplate(
            model_sha256=MODEL_SHA256,
            enrollment_count=3,
            embedding=unit_embedding(7),
        )

        payload = encode_template(original)

        self.assertEqual(len(payload), TEMPLATE_BYTES)
        self.assertEqual(decode_template(payload), original)
        self.assertEqual(encode_template(decode_template(payload)), payload)
        self.assertFalse(payload.startswith(b"\x80"))

    def test_rejects_wrong_shape_non_finite_or_non_unit_embedding(self):
        invalid_embeddings = (
            (0.0,) * (EMBEDDING_DIMENSION - 1),
            (0.0,) * (EMBEDDING_DIMENSION - 1) + (math.nan,),
            (0.5,) * EMBEDDING_DIMENSION,
            (0.0,) * EMBEDDING_DIMENSION,
        )
        for embedding in invalid_embeddings:
            with self.subTest(length=len(embedding)):
                with self.assertRaises(ValueError):
                    encode_template(
                        SpeakerTemplate(
                            model_sha256=MODEL_SHA256,
                            enrollment_count=1,
                            embedding=embedding,
                        )
                    )

    def test_rejects_invalid_model_hash_and_enrollment_count(self):
        for model_hash, count in (("", 1), ("zz" * 32, 1), ("ab" * 31, 1), (MODEL_SHA256, 0), (MODEL_SHA256, 17)):
            with self.subTest(model_hash=model_hash, count=count):
                with self.assertRaises(ValueError):
                    encode_template(
                        SpeakerTemplate(
                            model_sha256=model_hash,
                            enrollment_count=count,
                            embedding=unit_embedding(),
                        )
                    )

    def test_decode_rejects_unknown_or_malformed_payload(self):
        valid = bytearray(
            encode_template(
                SpeakerTemplate(
                    model_sha256=MODEL_SHA256,
                    enrollment_count=1,
                    embedding=unit_embedding(),
                )
            )
        )
        mutations = []
        for offset, replacement in (
            (0, b"X"),
            (8, struct.pack(">H", 2)),
            (10, struct.pack(">H", 191)),
            (12, struct.pack(">I", 8_000)),
            (18, struct.pack(">H", 1)),
            (20, b"\x00" * 32),
            (52, struct.pack(">f", math.nan)),
        ):
            changed = bytearray(valid)
            changed[offset : offset + len(replacement)] = replacement
            mutations.append(bytes(changed))
        mutations.extend((bytes(valid[:-1]), bytes(valid) + b"\x00"))

        for payload in mutations:
            with self.subTest(length=len(payload), prefix=payload[:12]):
                with self.assertRaises(ValueError):
                    decode_template(payload)


if __name__ == "__main__":
    unittest.main()
