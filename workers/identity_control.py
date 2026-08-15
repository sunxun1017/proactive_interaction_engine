"""Private, bounded identity worker control and evidence transport helpers."""

from dataclasses import dataclass
import datetime
from enum import Enum
import math
import threading
import uuid

import grpc
from google.protobuf import duration_pb2, timestamp_pb2
from proactive.platform.v1 import capability_pb2, identity_pb2, identity_worker_pb2


UTC = datetime.timezone.utc
_MAX_IDENTIFIER_BYTES = 128
_MAX_TEMPLATES = 128
_MAX_TEMPLATE_BYTES = 4 << 20
_MAX_TOTAL_TEMPLATE_BYTES = 16 << 20
_MAX_EVIDENCE_TTL = datetime.timedelta(seconds=10)


@dataclass
class MaterializedTemplate:
    profile_ref: str
    encoded: bytearray

    def clear(self):
        self.encoded[:] = bytes(len(self.encoded))


@dataclass
class VisionDesiredWork:
    generation: int
    revision: int
    evidence_window_id: str
    opened_at: datetime.datetime
    deadline: datetime.datetime
    detection: bool
    identification: bool
    identification_templates: list
    liveness: bool

    def clear(self):
        for template in self.identification_templates:
            template.clear()
        self.identification_templates.clear()


@dataclass
class AudioDesiredWork:
    generation: int
    revision: int
    evidence_window_id: str
    opened_at: datetime.datetime
    deadline: datetime.datetime
    kind: str
    challenge_id: str
    identification_templates: list
    expected_encoded: bytearray

    def clear(self):
        for template in self.identification_templates:
            template.clear()
        self.identification_templates.clear()
        self.expected_encoded[:] = bytes(len(self.expected_encoded))


class LatestIdentityWork:
    """Materializes one stream's latest desired state without replay."""

    def __init__(self, stack, *, utcnow=None, on_change=None):
        if stack not in ("vision", "audio"):
            raise ValueError("identity control stack must be vision or audio")
        self._stack = stack
        self._utcnow = utcnow or (lambda: datetime.datetime.now(UTC))
        self._on_change = on_change
        self._lock = threading.Lock()
        self._generation = 0
        self._last_revision = 0
        self._active = False
        self._work = None
        self._claimed = None
        self._closed = False

    def begin_stream(self):
        with self._lock:
            if self._closed:
                raise RuntimeError("identity desired state is closed")
            self._generation += 1
            self._last_revision = 0
            self._replace_locked(None, False)
        self._notify()

    def disconnect(self):
        with self._lock:
            self._replace_locked(None, False)
        self._notify()

    def apply(self, snapshot):
        expected_type = (
            identity_worker_pb2.WatchVisionResponse
            if self._stack == "vision"
            else identity_worker_pb2.WatchAudioResponse
        )
        if not isinstance(snapshot, expected_type):
            raise ValueError("identity control snapshot is for the wrong stack")
        state = snapshot.WhichOneof("state")
        if snapshot.revision <= 0 or state not in ("idle", "active"):
            raise ValueError("identity control revision and strict state are required")
        with self._lock:
            if self._closed:
                raise RuntimeError("identity desired state is closed")
            if snapshot.revision <= self._last_revision:
                raise ValueError("identity control revision must strictly increase")
            self._last_revision = snapshot.revision
            if state == "idle":
                self._replace_locked(None, False)
            else:
                work = self._materialize_active(snapshot.revision, snapshot.active)
                if not self._now().before(work.deadline):
                    work.clear()
                    work = None
                self._replace_locked(work, work is not None)
        self._notify()

    def claim(self):
        with self._lock:
            if self._closed or not self._active or self._work is None or self._claimed is not None:
                return None
            work = self._work
            self._work = None
            self._claimed = work
            return work

    def release(self, work):
        if work is None:
            return
        with self._lock:
            if self._claimed is work:
                self._claimed = None
        work.clear()

    def is_current(self, work):
        if work is None:
            return False
        with self._lock:
            return (
                not self._closed
                and self._active
                and work.generation == self._generation
                and work.revision == self._last_revision
                and not self._now().before(work.opened_at)
                and self._now().before(work.deadline)
            )

    def close(self):
        with self._lock:
            if self._closed:
                return
            self._closed = True
            self._replace_locked(None, False)
        self._notify()

    def _replace_locked(self, work, active):
        if self._work is not None:
            self._work.clear()
        self._work = work
        self._active = active

    def _materialize_active(self, revision, active):
        if self._stack == "vision":
            window_id, opened_at, deadline = _window(active)
            if not (active.HasField("detection") or active.HasField("identification") or active.HasField("liveness")):
                raise ValueError("vision active work requires a task")
            if (active.HasField("identification") or active.HasField("liveness")) and not active.HasField("detection"):
                raise ValueError("face identification and liveness require detection")
            templates = _templates(active.identification.templates) if active.HasField("identification") else []
            return VisionDesiredWork(
                self._generation,
                revision,
                window_id,
                opened_at,
                deadline,
                active.HasField("detection"),
                active.HasField("identification"),
                templates,
                active.HasField("liveness"),
            )

        task = active.WhichOneof("task")
        if task not in ("identification", "verification"):
            raise ValueError("audio active work requires exactly one task")
        wire = getattr(active, task)
        window_id, opened_at, deadline = _window(wire)
        if task == "identification":
            return AudioDesiredWork(
                self._generation,
                revision,
                window_id,
                opened_at,
                deadline,
                task,
                "",
                _templates(wire.templates),
                bytearray(),
            )
        if not _valid_identifier(wire.verification_challenge_id):
            raise ValueError("verification challenge id is invalid")
        expected = _encoded_template(wire.expected_encoded_template)
        return AudioDesiredWork(
            self._generation,
            revision,
            window_id,
            opened_at,
            deadline,
            task,
            wire.verification_challenge_id,
            [],
            expected,
        )

    def _now(self):
        return _AwareTime(self._utcnow())

    def _notify(self):
        if self._on_change is not None:
            self._on_change()


class _AwareTime:
    def __init__(self, value):
        if not isinstance(value, datetime.datetime) or value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("UTC clock must return timezone-aware values")
        self.value = value.astimezone(UTC)

    def before(self, other):
        return self.value < other


class IdentityControlStreamClient:
    """Own one cancelable Watch call and reconnect without replay."""

    def __init__(self, stub, stack, source_instance_id, binding_supplier, latest):
        if stub is None or stack not in ("vision", "audio") or not _valid_identifier(source_instance_id):
            raise ValueError("control stub, stack, and source instance are required")
        if not callable(binding_supplier) or not isinstance(latest, LatestIdentityWork):
            raise ValueError("binding supplier and latest desired state are required")
        self._stub = stub
        self._stack = stack
        self._source_instance_id = source_instance_id
        self._binding_supplier = binding_supplier
        self._latest = latest
        self._lock = threading.Lock()
        self._call = None

    def run(self, stop_event, reconnect_delay):
        if not isinstance(stop_event, threading.Event) or reconnect_delay <= 0:
            raise ValueError("stop event and positive reconnect delay are required")
        method = self._stub.WatchVision if self._stack == "vision" else self._stub.WatchAudio
        while not stop_event.is_set():
            self._latest.begin_stream()
            try:
                call = method(self._request())
                with self._lock:
                    self._call = call
                if stop_event.is_set():
                    call.cancel()
                for snapshot in call:
                    if stop_event.is_set():
                        break
                    self._latest.apply(snapshot)
            except (grpc.RpcError, ValueError):
                pass
            finally:
                with self._lock:
                    current = self._call
                    self._call = None
                if current is not None:
                    current.cancel()
                self._latest.disconnect()
            if stop_event.wait(reconnect_delay):
                break

    def cancel(self):
        with self._lock:
            call = self._call
        if call is not None:
            call.cancel()

    def _request(self):
        raw = tuple(self._binding_supplier())
        seen_capabilities = set()
        seen_leases = set()
        bindings = []
        for item in raw:
            capability = item.capability
            lease_id = item.lease_id
            if not item.active or capability in seen_capabilities or lease_id in seen_leases or not _valid_identifier(lease_id):
                raise ValueError("identity control lease binding is invalid")
            seen_capabilities.add(capability)
            seen_leases.add(lease_id)
            bindings.append(identity_worker_pb2.IdentityWorkerLeaseBinding(
                capability=capability, provider_lease_id=lease_id
            ))
        base = (
            capability_pb2.SERVICE_CAPABILITY_KIND_PERSON_PRESENCE
            if self._stack == "vision"
            else capability_pb2.SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY
        )
        if base not in seen_capabilities:
            raise ValueError("identity control base lease is required")
        request_type = (
            identity_worker_pb2.WatchVisionRequest
            if self._stack == "vision"
            else identity_worker_pb2.WatchAudioRequest
        )
        return request_type(source_instance_id=self._source_instance_id, lease_bindings=bindings)


@dataclass(frozen=True)
class EvidenceCandidate:
    candidate_id: str
    profile_ref: str
    score: float
    model_version: str


@dataclass(frozen=True)
class VerificationCandidate:
    candidate_id: str
    score: float
    model_version: str


class PublishDisposition(Enum):
    SUCCEEDED = "SUCCEEDED"
    RETRYABLE = "RETRYABLE"
    TERMINAL = "TERMINAL"


@dataclass
class _PendingEvidence:
    method: str
    request: object
    deadline: datetime.datetime
    timeout: float


class IdentityEvidencePublisher:
    """Publishes typed evidence with one exact retry slot per capability."""

    def __init__(self, stub, sessions, evidence_ttl, *, utcnow=None, uuid_factory=None):
        if stub is None or not isinstance(sessions, dict) or not sessions:
            raise ValueError("identity stub and provider sessions are required")
        if not isinstance(evidence_ttl, datetime.timedelta) or not datetime.timedelta(0) < evidence_ttl <= _MAX_EVIDENCE_TTL:
            raise ValueError("identity evidence TTL must be within (0,10s]")
        for capability, session in sessions.items():
            if session is None or session.capability != capability:
                raise ValueError("identity provider session does not match capability")
        self._stub = stub
        self._sessions = dict(sessions)
        self._ttl = evidence_ttl
        self._utcnow = utcnow or (lambda: datetime.datetime.now(UTC))
        self._uuid_factory = uuid_factory or uuid.uuid4
        self._pending = {}
        self._lock = threading.Lock()

    def publish_face_detection(self, window_id, occurred_at, deadline, faces_observed):
        if type(faces_observed) is not int or not 0 <= faces_observed <= (1 << 32) - 1:
            raise ValueError("face count is invalid")
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION
        return self._publish(capability, "PublishFaceDetectionEvidence", occurred_at, deadline, window_id,
            lambda metadata: identity_pb2.PublishFaceDetectionEvidenceRequest(
                metadata=metadata, faces_observed=faces_observed
            ))

    def publish_face_identification(self, window_id, occurred_at, deadline, candidates):
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION
        mapped = [_face_candidate(item) for item in _candidate_list(candidates)]
        return self._publish(capability, "PublishFaceIdentificationEvidence", occurred_at, deadline, window_id,
            lambda metadata: identity_pb2.PublishFaceIdentificationEvidenceRequest(
                metadata=metadata, candidates=mapped
            ))

    def publish_face_liveness(self, window_id, occurred_at, deadline, state):
        if state not in (
            identity_pb2.FACE_LIVENESS_STATE_UNKNOWN,
            identity_pb2.FACE_LIVENESS_STATE_PASSED,
            identity_pb2.FACE_LIVENESS_STATE_FAILED,
        ):
            raise ValueError("face liveness state is invalid")
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_FACE_LIVENESS
        return self._publish(capability, "PublishFaceLivenessEvidence", occurred_at, deadline, window_id,
            lambda metadata: identity_pb2.PublishFaceLivenessEvidenceRequest(metadata=metadata, state=state))

    def publish_speaker_identification(self, window_id, occurred_at, deadline, candidates):
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION
        mapped = [_speaker_candidate(item) for item in _candidate_list(candidates)]
        return self._publish(capability, "PublishSpeakerIdentificationEvidence", occurred_at, deadline, window_id,
            lambda metadata: identity_pb2.PublishSpeakerIdentificationEvidenceRequest(
                metadata=metadata, candidates=mapped
            ))

    def publish_speaker_verification(self, challenge_id, window_id, occurred_at, deadline, candidate):
        if not _valid_identifier(challenge_id) or not isinstance(candidate, VerificationCandidate):
            raise ValueError("verification challenge and candidate are required")
        _validate_candidate(candidate.candidate_id, "", candidate.score, candidate.model_version, False)
        capability = capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION
        return self._publish(capability, "PublishSpeakerVerificationEvidence", occurred_at, deadline, window_id,
            lambda metadata: identity_pb2.PublishSpeakerVerificationEvidenceRequest(
                metadata=metadata,
                verification_challenge_id=challenge_id,
                candidate_id=candidate.candidate_id,
                score=candidate.score,
                model_version=candidate.model_version,
            ))

    def retry(self, capability):
        with self._lock:
            pending = self._pending.get(capability)
            if pending is None:
                return PublishDisposition.TERMINAL
            if not self._now() < pending.deadline:
                del self._pending[capability]
                return PublishDisposition.TERMINAL
            return self._attempt_locked(capability, pending)

    def has_pending(self, capability):
        with self._lock:
            return capability in self._pending

    def discard(self, capability):
        with self._lock:
            self._pending.pop(capability, None)

    def discard_all(self):
        with self._lock:
            self._pending.clear()

    def _publish(self, capability, method, occurred_at, deadline, window_id, request_factory):
        _validate_window(occurred_at, deadline, window_id, self._now())
        with self._lock:
            if capability in self._pending:
                raise RuntimeError("capability already has pending evidence")
            session = self._sessions.get(capability)
            if session is None or not session.active:
                return PublishDisposition.TERMINAL
            fragment_id = str(self._uuid_factory())
            sequence = session.next_source_sequence()
            occurred = timestamp_pb2.Timestamp()
            occurred.FromDatetime(occurred_at.astimezone(UTC))
            ttl = duration_pb2.Duration()
            ttl.FromTimedelta(self._ttl)
            metadata = identity_pb2.IdentityEvidenceMetadata(
                fragment_id=fragment_id,
                provider_lease_id=session.lease_id,
                source_instance_id=session.instance_id,
                source_seq=sequence,
                occurred_at=occurred,
                ttl=ttl,
                trace_id=f"trace:{fragment_id}",
                evidence_window_id=window_id,
            )
            pending = _PendingEvidence(method, request_factory(metadata), deadline, session.rpc_timeout)
            self._pending[capability] = pending
            return self._attempt_locked(capability, pending)

    def _attempt_locked(self, capability, pending):
        try:
            response = getattr(self._stub, pending.method)(pending.request, timeout=pending.timeout)
        except grpc.RpcError as error:
            if error.code() in (grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED):
                return PublishDisposition.RETRYABLE
            self._pending.pop(capability, None)
            return PublishDisposition.TERMINAL
        if response is None or not response.HasField("receipt"):
            self._pending.pop(capability, None)
            return PublishDisposition.TERMINAL
        receipt = response.receipt
        if receipt.fragment_id != pending.request.metadata.fragment_id:
            self._pending.pop(capability, None)
            return PublishDisposition.TERMINAL
        self._pending.pop(capability, None)
        if receipt.status in (
            identity_pb2.IDENTITY_EVIDENCE_RECEIPT_STATUS_ACCEPTED,
            identity_pb2.IDENTITY_EVIDENCE_RECEIPT_STATUS_DUPLICATE,
        ):
            return PublishDisposition.SUCCEEDED
        return PublishDisposition.TERMINAL

    def _now(self):
        value = self._utcnow()
        if not isinstance(value, datetime.datetime) or value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("UTC clock must return timezone-aware values")
        return value.astimezone(UTC)


def _window(active):
    if not _valid_identifier(active.evidence_window_id):
        raise ValueError("evidence window id is invalid")
    try:
        opened_at = active.opened_at.ToDatetime(tzinfo=UTC)
        deadline = active.deadline.ToDatetime(tzinfo=UTC)
    except (AttributeError, ValueError) as error:
        raise ValueError("identity work times are invalid") from error
    if not opened_at < deadline:
        raise ValueError("identity work interval must be half-open and positive")
    return active.evidence_window_id, opened_at, deadline


def _templates(wire_templates):
    if len(wire_templates) > _MAX_TEMPLATES:
        raise ValueError("identity template count exceeds bound")
    output = []
    seen = set()
    total = 0
    try:
        for wire in wire_templates:
            if not _valid_identifier(wire.profile_ref) or wire.profile_ref in seen:
                raise ValueError("identity template profile is invalid")
            encoded = _encoded_template(wire.encoded_template)
            total += len(encoded)
            if total > _MAX_TOTAL_TEMPLATE_BYTES:
                encoded[:] = bytes(len(encoded))
                raise ValueError("identity template total bytes exceed bound")
            seen.add(wire.profile_ref)
            output.append(MaterializedTemplate(wire.profile_ref, encoded))
    except Exception:
        for item in output:
            item.clear()
        raise
    return output


def _encoded_template(value):
    if not isinstance(value, bytes) or not 0 < len(value) <= _MAX_TEMPLATE_BYTES:
        raise ValueError("encoded identity template is invalid")
    return bytearray(value)


def _candidate_list(candidates):
    output = tuple(candidates)
    if len(output) > 16:
        raise ValueError("identity candidate count exceeds bound")
    seen = set()
    for item in output:
        if not isinstance(item, EvidenceCandidate):
            raise ValueError("identity candidate is invalid")
        _validate_candidate(item.candidate_id, item.profile_ref, item.score, item.model_version, True)
        if item.candidate_id in seen:
            raise ValueError("identity candidate id is duplicated")
        seen.add(item.candidate_id)
    return output


def _face_candidate(item):
    return identity_pb2.FaceIdentificationCandidate(
        candidate_id=item.candidate_id,
        profile_ref=item.profile_ref,
        score=item.score,
        model_version=item.model_version,
    )


def _speaker_candidate(item):
    return identity_pb2.SpeakerIdentificationCandidate(
        candidate_id=item.candidate_id,
        profile_ref=item.profile_ref,
        score=item.score,
        model_version=item.model_version,
    )


def _validate_candidate(candidate_id, profile_ref, score, model_version, require_profile):
    if not _valid_identifier(candidate_id) or not _valid_identifier(model_version):
        raise ValueError("candidate id and model version are invalid")
    if require_profile and not _valid_identifier(profile_ref):
        raise ValueError("candidate profile is invalid")
    if isinstance(score, bool) or not isinstance(score, (int, float)) or not math.isfinite(score) or not 0 <= score <= 1:
        raise ValueError("candidate score is outside [0,1]")


def _validate_window(occurred_at, deadline, window_id, now):
    if not _valid_identifier(window_id):
        raise ValueError("evidence window id is invalid")
    for value in (occurred_at, deadline):
        if not isinstance(value, datetime.datetime) or value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("evidence times must be timezone-aware")
    occurred_at = occurred_at.astimezone(UTC)
    deadline = deadline.astimezone(UTC)
    if occurred_at > now or not occurred_at < deadline or not now < deadline:
        raise ValueError("evidence is outside its half-open work interval")


def _valid_identifier(value):
    return (
        isinstance(value, str)
        and bool(value)
        and value.strip() == value
        and len(value.encode("utf-8")) <= _MAX_IDENTIFIER_BYTES
    )
