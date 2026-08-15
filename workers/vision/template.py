"""Fixed binary encoding for SFace templates; never pickle model output."""

from dataclasses import dataclass
import math
import struct

import numpy as np


FACE_TEMPLATE_DIMENSION = 128
FACE_TEMPLATE_FORMAT_VERSION = 1
FACE_TEMPLATE_PROFILE_VERSION = 1
FACE_TEMPLATE_MODEL_SHA256 = (
    "0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79"
)
_MAGIC = b"PIEFACE1"
_FLOAT32_LE_L2_UNIT = 0x0101
_HEADER = struct.Struct("<8sHHHHH32s")
_UNIT_TOLERANCE = 1e-4


@dataclass(frozen=True)
class FaceTemplate:
    embedding: np.ndarray
    sample_count: int


def normalize_embedding(embedding):
    """Return one finite 128-dimensional L2-unit float32 embedding."""
    vector = _finite_vector(embedding)
    norm = float(np.linalg.norm(vector.astype(np.float64)))
    if not math.isfinite(norm) or norm <= 0:
        raise ValueError("face embedding norm must be finite and positive")
    normalized = (vector.astype(np.float64) / norm).astype(np.float32)
    final_norm = float(np.linalg.norm(normalized.astype(np.float64)))
    if not math.isfinite(final_norm) or final_norm <= 0:
        raise ValueError("normalized face embedding is invalid")
    normalized = (normalized.astype(np.float64) / final_norm).astype(np.float32)
    normalized.setflags(write=False)
    return normalized


def aggregate_embeddings(embeddings):
    """Normalize samples independently, average them, then normalize the mean."""
    samples = tuple(normalize_embedding(embedding) for embedding in embeddings)
    if not samples:
        raise ValueError("at least one face embedding is required")
    if len(samples) > 65535:
        raise ValueError("face template sample count exceeds format capacity")
    mean = np.mean(np.stack(samples).astype(np.float64), axis=0)
    return FaceTemplate(embedding=normalize_embedding(mean), sample_count=len(samples))


def encode_face_template(template):
    """Encode one validated template into the fixed little-endian v1 format."""
    if not isinstance(template, FaceTemplate):
        raise ValueError("face template is required")
    if type(template.sample_count) is not int or not 1 <= template.sample_count <= 65535:
        raise ValueError("face template sample count must be within [1, 65535]")
    vector = _finite_vector(template.embedding)
    norm = float(np.linalg.norm(vector.astype(np.float64)))
    if not math.isclose(norm, 1.0, rel_tol=0.0, abs_tol=_UNIT_TOLERANCE):
        raise ValueError("face template embedding must be L2-unit normalized")
    header = _HEADER.pack(
        _MAGIC,
        FACE_TEMPLATE_FORMAT_VERSION,
        FACE_TEMPLATE_PROFILE_VERSION,
        FACE_TEMPLATE_DIMENSION,
        template.sample_count,
        _FLOAT32_LE_L2_UNIT,
        bytes.fromhex(FACE_TEMPLATE_MODEL_SHA256),
    )
    return header + vector.astype("<f4", copy=False).tobytes(order="C")


def decode_face_template(encoded):
    """Decode only the exact non-extensible v1 representation."""
    if not isinstance(encoded, bytes):
        raise ValueError("encoded face template must be bytes")
    expected_size = _HEADER.size + FACE_TEMPLATE_DIMENSION * 4
    if len(encoded) != expected_size:
        raise ValueError("encoded face template has invalid size")
    magic, version, profile_version, dimension, sample_count, flags, model_sha256 = (
        _HEADER.unpack_from(encoded)
    )
    if magic != _MAGIC:
        raise ValueError("encoded face template has invalid magic")
    if version != FACE_TEMPLATE_FORMAT_VERSION:
        raise ValueError("encoded face template version is unsupported")
    if profile_version != FACE_TEMPLATE_PROFILE_VERSION:
        raise ValueError("encoded face template profile is unsupported")
    if dimension != FACE_TEMPLATE_DIMENSION:
        raise ValueError("encoded face template dimension is unsupported")
    if sample_count == 0:
        raise ValueError("encoded face template sample count is invalid")
    if flags != _FLOAT32_LE_L2_UNIT:
        raise ValueError("encoded face template dtype or normalization is unsupported")
    if model_sha256 != bytes.fromhex(FACE_TEMPLATE_MODEL_SHA256):
        raise ValueError("encoded face template model is unsupported")
    vector = np.frombuffer(encoded, dtype="<f4", offset=_HEADER.size).astype(np.float32, copy=True)
    _finite_vector(vector)
    norm = float(np.linalg.norm(vector.astype(np.float64)))
    if not math.isclose(norm, 1.0, rel_tol=0.0, abs_tol=_UNIT_TOLERANCE):
        raise ValueError("encoded face template embedding is not L2-unit normalized")
    vector.setflags(write=False)
    return FaceTemplate(embedding=vector, sample_count=sample_count)


def _finite_vector(embedding):
    try:
        vector = np.asarray(embedding, dtype=np.float32)
    except (TypeError, ValueError, OverflowError) as error:
        raise ValueError("face embedding must be numeric") from error
    if vector.shape != (FACE_TEMPLATE_DIMENSION,):
        raise ValueError(f"face embedding must have dimension {FACE_TEMPLATE_DIMENSION}")
    if not np.isfinite(vector).all():
        raise ValueError("face embedding must contain only finite values")
    return np.array(vector, dtype=np.float32, copy=True)
