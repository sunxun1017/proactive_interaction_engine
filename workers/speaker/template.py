"""Versioned, fixed-width speaker templates for the encrypted biometric vault."""

from dataclasses import dataclass
import math
import struct


TEMPLATE_SCHEMA = "speaker-template-v1"
EMBEDDING_DIMENSION = 192
SAMPLE_RATE = 16_000
MAX_ENROLLMENT_EMBEDDINGS = 16

_MAGIC = b"SPKRTPL1"
_VERSION = 1
_RESERVED = 0
_HEADER = struct.Struct(">8sHHIHH32s")
_EMBEDDING = struct.Struct(f">{EMBEDDING_DIMENSION}f")
TEMPLATE_BYTES = _HEADER.size + _EMBEDDING.size
_UNIT_NORM_TOLERANCE = 1e-4


@dataclass(frozen=True)
class SpeakerTemplate:
    model_sha256: str
    enrollment_count: int
    embedding: tuple[float, ...]


def encode_template(template):
    """Encode one canonical template without pickle or executable metadata."""
    if not isinstance(template, SpeakerTemplate):
        raise ValueError("speaker template is required")
    model_digest = _model_digest(template.model_sha256)
    _validate_count(template.enrollment_count)
    embedding = _validated_embedding(template.embedding)
    return _HEADER.pack(
        _MAGIC,
        _VERSION,
        EMBEDDING_DIMENSION,
        SAMPLE_RATE,
        template.enrollment_count,
        _RESERVED,
        model_digest,
    ) + _EMBEDDING.pack(*embedding)


def decode_template(payload):
    """Decode only the single supported speaker-template-v1 representation."""
    if not isinstance(payload, bytes) or len(payload) != TEMPLATE_BYTES:
        raise ValueError("speaker template has an invalid size")
    magic, version, dimension, sample_rate, count, reserved, digest = _HEADER.unpack_from(payload)
    if magic != _MAGIC or version != _VERSION:
        raise ValueError("speaker template schema is unsupported")
    if dimension != EMBEDDING_DIMENSION or sample_rate != SAMPLE_RATE or reserved != _RESERVED:
        raise ValueError("speaker template header is invalid")
    if not any(digest):
        raise ValueError("speaker template model digest is invalid")
    _validate_count(count)
    embedding = _validated_embedding(_EMBEDDING.unpack_from(payload, _HEADER.size))
    return SpeakerTemplate(
        model_sha256=digest.hex(),
        enrollment_count=count,
        embedding=embedding,
    )


def _model_digest(value):
    if not isinstance(value, str) or len(value) != 64 or value != value.lower():
        raise ValueError("speaker template model digest is invalid")
    try:
        digest = bytes.fromhex(value)
    except ValueError as exc:
        raise ValueError("speaker template model digest is invalid") from exc
    if len(digest) != 32 or not any(digest):
        raise ValueError("speaker template model digest is invalid")
    return digest


def _validate_count(value):
    if not isinstance(value, int) or isinstance(value, bool) or not 1 <= value <= MAX_ENROLLMENT_EMBEDDINGS:
        raise ValueError("speaker template enrollment count is invalid")


def _validated_embedding(values):
    try:
        embedding = tuple(float(value) for value in values)
    except (TypeError, ValueError) as exc:
        raise ValueError("speaker template embedding is invalid") from exc
    if len(embedding) != EMBEDDING_DIMENSION or not all(math.isfinite(value) for value in embedding):
        raise ValueError("speaker template embedding is invalid")
    norm = math.sqrt(sum(value * value for value in embedding))
    if not math.isclose(norm, 1.0, rel_tol=0.0, abs_tol=_UNIT_NORM_TOLERANCE):
        raise ValueError("speaker template embedding is not L2 normalized")
    return embedding
