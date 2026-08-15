"""Model-independent speaker enrollment and scoring over injected embeddings."""

import math

from workers.speaker.template import (
    EMBEDDING_DIMENSION,
    MAX_ENROLLMENT_EMBEDDINGS,
    SAMPLE_RATE,
    SpeakerTemplate,
    decode_template,
    encode_template,
)


_COSINE_TOLERANCE = 1e-9


class SpeakerModel:
    """Turns local PCM into templates and unthresholded normalized scores."""

    def __init__(self, backend):
        if backend is None:
            raise ValueError("speaker embedding backend is required")
        model_sha256 = getattr(backend, "model_sha256", "")
        if not _valid_sha256(model_sha256):
            raise ValueError("speaker backend model digest is invalid")
        self._backend = backend
        self._model_sha256 = model_sha256

    @property
    def model_sha256(self):
        return self._model_sha256

    def extract(self, pcm_s16le, sample_rate=SAMPLE_RATE):
        pcm = _validated_pcm(pcm_s16le, sample_rate)
        return normalize_embedding(self._backend.embed(pcm, sample_rate))

    def enroll(self, pcm_segments, sample_rate=SAMPLE_RATE):
        try:
            segments = iter(pcm_segments)
        except TypeError as exc:
            raise ValueError("speaker enrollment segments are invalid") from exc
        embeddings = []
        for segment in segments:
            if len(embeddings) == MAX_ENROLLMENT_EMBEDDINGS:
                raise ValueError("speaker enrollment segment count is invalid")
            embeddings.append(self.extract(segment, sample_rate))
        if not embeddings:
            raise ValueError("speaker enrollment segment count is invalid")
        centroid = normalize_embedding(
            tuple(
                sum(embedding[index] for embedding in embeddings) / len(embeddings)
                for index in range(EMBEDDING_DIMENSION)
            )
        )
        return encode_template(
            SpeakerTemplate(
                model_sha256=self._model_sha256,
                enrollment_count=len(embeddings),
                embedding=centroid,
            )
        )

    def score(self, pcm_s16le, encoded_template, sample_rate=SAMPLE_RATE):
        template = decode_template(encoded_template)
        if template.model_sha256 != self._model_sha256:
            raise ValueError("speaker template model does not match backend")
        candidate = self.extract(pcm_s16le, sample_rate)
        cosine = sum(left * right for left, right in zip(candidate, template.embedding))
        return affine_cosine_score(cosine)


def normalize_embedding(values):
    try:
        embedding = tuple(float(value) for value in values)
    except (TypeError, ValueError) as exc:
        raise ValueError("speaker embedding is invalid") from exc
    if len(embedding) != EMBEDDING_DIMENSION or not all(math.isfinite(value) for value in embedding):
        raise ValueError("speaker embedding is invalid")
    norm = math.sqrt(sum(value * value for value in embedding))
    if not math.isfinite(norm) or norm <= 0.0:
        raise ValueError("speaker embedding norm is invalid")
    return tuple(value / norm for value in embedding)


def affine_cosine_score(cosine):
    """Map cosine [-1, 1] to [0, 1]; this is not a probability or threshold."""
    try:
        value = float(cosine)
    except (TypeError, ValueError) as exc:
        raise ValueError("speaker cosine score is invalid") from exc
    if not math.isfinite(value) or value < -1.0 - _COSINE_TOLERANCE or value > 1.0 + _COSINE_TOLERANCE:
        raise ValueError("speaker cosine score is invalid")
    value = min(1.0, max(-1.0, value))
    return (value + 1.0) / 2.0


def _validated_pcm(value, sample_rate):
    if sample_rate != SAMPLE_RATE:
        raise ValueError("speaker PCM must be 16 kHz")
    if not isinstance(value, (bytes, bytearray, memoryview)):
        raise ValueError("speaker PCM must be s16le bytes")
    pcm = bytes(value)
    if not pcm or len(pcm) % 2 != 0:
        raise ValueError("speaker PCM must contain complete s16le samples")
    return pcm


def _valid_sha256(value):
    if not isinstance(value, str) or len(value) != 64 or value != value.lower():
        return False
    try:
        return len(bytes.fromhex(value)) == 32 and any(bytes.fromhex(value))
    except ValueError:
        return False
