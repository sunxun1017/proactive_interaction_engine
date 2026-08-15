"""Single-camera owner combining continuous anonymous and bounded identity work."""

from dataclasses import dataclass
import datetime
import threading
import time

from proactive.platform.v1 import capability_pb2, identity_pb2

from workers.identity_control import EvidenceCandidate, LatestIdentityWork


UTC = datetime.timezone.utc


@dataclass(frozen=True)
class VisionInferenceResult:
    faces_observed: int
    candidates: tuple = ()
    liveness: int = identity_pb2.FACE_LIVENESS_STATE_UNKNOWN

    def __post_init__(self):
        if type(self.faces_observed) is not int or not 0 <= self.faces_observed <= (1 << 32) - 1:
            raise ValueError("vision inference face count is invalid")
        candidates = tuple(self.candidates)
        if any(not isinstance(item, EvidenceCandidate) for item in candidates):
            raise ValueError("vision inference candidates are invalid")
        if self.liveness not in (
            identity_pb2.FACE_LIVENESS_STATE_UNKNOWN,
            identity_pb2.FACE_LIVENESS_STATE_PASSED,
            identity_pb2.FACE_LIVENESS_STATE_FAILED,
        ):
            raise ValueError("vision inference liveness is invalid")
        object.__setattr__(self, "candidates", candidates)


@dataclass
class _Frame:
    occurred_at: datetime.datetime
    value: object

    def clear(self):
        _wipe(self.value)
        self.value = None


class _LatestFrame:
    def __init__(self):
        self._lock = threading.Lock()
        self._frame = None
        self._closed = False

    def put(self, frame):
        with self._lock:
            if self._closed:
                frame.clear()
                return
            if self._frame is not None:
                self._frame.clear()
            self._frame = frame

    def take(self, opened_at, deadline):
        with self._lock:
            frame = self._frame
            if frame is None:
                return None
            if frame.occurred_at < opened_at:
                frame.clear()
                self._frame = None
                return None
            if not frame.occurred_at < deadline:
                frame.clear()
                self._frame = None
                return None
            self._frame = None
            return frame

    @property
    def size(self):
        with self._lock:
            return int(self._frame is not None)

    def close(self):
        with self._lock:
            self._closed = True
            if self._frame is not None:
                self._frame.clear()
                self._frame = None


class VisionStack:
    """Own one camera; identity inference is injected and never concurrent."""

    def __init__(
        self,
        source,
        presence_detector,
        presence_gate,
        base_session,
        identity_sessions,
        desired,
        publisher,
        inference,
        *,
        utcnow,
        clone_frame,
        join_timeout,
        control_client=None,
        reconnect_delay=None,
    ):
        if any(item is None for item in (source, presence_detector, presence_gate, base_session, desired, publisher, inference)):
            raise ValueError("vision stack dependencies are required")
        if not isinstance(desired, LatestIdentityWork) or not callable(utcnow) or not callable(clone_frame):
            raise ValueError("vision desired state, clock, and frame clone are required")
        if join_timeout <= 0 or (control_client is None) != (reconnect_delay is None):
            raise ValueError("positive join timeout and complete control configuration are required")
        if reconnect_delay is not None and reconnect_delay <= 0:
            raise ValueError("control reconnect delay must be positive")
        self._source = source
        self._presence_detector = presence_detector
        self._presence_gate = presence_gate
        self._base_session = base_session
        self._identity_sessions = tuple(identity_sessions)
        self._desired = desired
        self._publisher = publisher
        self._inference = inference
        self._utcnow = utcnow
        self._clone_frame = clone_frame
        self._join_timeout = join_timeout
        self._control_client = control_client
        self._reconnect_delay = reconnect_delay
        self._frames = _LatestFrame()
        self._pending_work = None
        self._pending_lock = threading.Lock()
        self._inference_lock = threading.Lock()
        self._wake = threading.Event()
        self._threads = []
        self._shutdown_requested = False
        self._finalized = False
        self._run_stop_event = None
        self._capture_done = threading.Event()
        self._capture_done.set()
        self._close_lock = threading.Lock()

    @property
    def buffered_frames(self):
        return self._frames.size

    @property
    def owned_threads_alive(self):
        return sum(thread.is_alive() for thread in self._threads)

    def capture_once(self):
        with self._close_lock:
            if self._shutdown_requested:
                return False
        active = self._base_session.maintain()
        for session in self._identity_sessions:
            session.maintain()
        ok, frame = self._source.read()
        if not ok or frame is None:
            return False
        occurred_at = self._now()
        try:
            transition = self._presence_gate.update(self._presence_detector(frame))
            copied = self._clone_frame(frame)
            if copied is frame:
                raise ValueError("vision frame clone must have independent ownership")
            self._frames.put(_Frame(occurred_at, copied))
            self._wake.set()
            if transition is not None and active:
                self._base_session.publish_presence(transition)
            return True
        finally:
            frame = None

    def process_identity_once(self):
        with self._pending_lock:
            if self._pending_work is None:
                self._pending_work = self._desired.claim()
            work = self._pending_work
        if work is None:
            return False
        if not self._desired.is_current(work):
            self._discard_work(work)
            return False
        frame = self._frames.take(work.opened_at, work.deadline)
        if frame is None:
            return False
        try:
            with self._inference_lock:
                result = self._inference.infer(work, frame.value)
            if not isinstance(result, VisionInferenceResult):
                raise ValueError("vision inference returned an invalid result")
            if not self._desired.is_current(work):
                return False
            if work.detection:
                self._publisher.publish_face_detection(
                    work.evidence_window_id, frame.occurred_at, work.deadline, result.faces_observed
                )
            if work.identification:
                self._publisher.publish_face_identification(
                    work.evidence_window_id, frame.occurred_at, work.deadline, result.candidates
                )
            if work.liveness:
                self._publisher.publish_face_liveness(
                    work.evidence_window_id, frame.occurred_at, work.deadline, result.liveness
                )
            return True
        except Exception:
            self._mark_identity_unavailable(work)
            return False
        finally:
            frame.clear()
            self._finish_work(work)

    def run(self, stop_event):
        if not isinstance(stop_event, threading.Event):
            raise ValueError("vision stop event is required")
        with self._close_lock:
            if self._shutdown_requested or self._run_stop_event is not None:
                raise RuntimeError("vision stack may only run once")
            self._run_stop_event = stop_event
            self._capture_done.clear()
        identity_thread = threading.Thread(
            target=self._identity_loop, args=(stop_event,), name="vision-identity", daemon=False
        )
        self._threads = [identity_thread]
        if self._control_client is not None:
            self._threads.append(threading.Thread(
                target=self._control_client.run,
                args=(stop_event, self._reconnect_delay),
                name="vision-control",
                daemon=False,
            ))
        for thread in self._threads:
            thread.start()
        try:
            while not stop_event.is_set() and self.capture_once():
                pass
        finally:
            self._capture_done.set()
            stop_event.set()
            self._request_shutdown()
            self._join_owned_threads(wait_for_capture=False)
            self._finalize_shutdown()

    def close(self):
        self._request_shutdown()
        self._join_owned_threads(wait_for_capture=True)
        self._finalize_shutdown()

    def _request_shutdown(self):
        with self._close_lock:
            if self._shutdown_requested:
                return
            self._shutdown_requested = True
            stop_event = self._run_stop_event
        if stop_event is not None:
            stop_event.set()
        if self._control_client is not None:
            self._control_client.cancel()
        self._wake.set()
        # Closing desired state invalidates the claimed revision but deliberately
        # leaves claimed bytes under executor ownership until it exits.
        self._desired.close()
        self._source.close()

    def _join_owned_threads(self, *, wait_for_capture):
        if any(thread.ident == threading.get_ident() for thread in self._threads):
            raise RuntimeError("vision stack cannot join from an owned thread")
        deadline = time.monotonic() + self._join_timeout
        if wait_for_capture:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not self._capture_done.wait(remaining):
                raise RuntimeError("vision stack failed to join capture owner")
        for thread in self._threads:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            thread.join(remaining)
        if self.owned_threads_alive:
            raise RuntimeError("vision stack failed to join owned threads")

    def _finalize_shutdown(self):
        with self._close_lock:
            if self._finalized:
                return
            if self.owned_threads_alive:
                raise RuntimeError("vision executor must exit before final cleanup")
            self._finalized = True
        self._frames.close()
        with self._pending_lock:
            work = self._pending_work
            self._pending_work = None
        if work is not None:
            self._desired.release(work)
        closed = set()
        for session in (self._base_session,) + self._identity_sessions:
            if id(session) not in closed:
                closed.add(id(session))
                session.close()

    def _identity_loop(self, stop_event):
        while not stop_event.is_set():
            self._wake.wait()
            self._wake.clear()
            if stop_event.is_set():
                return
            self.process_identity_once()

    def _finish_work(self, work):
        with self._pending_lock:
            if self._pending_work is work:
                self._pending_work = None
        self._desired.release(work)

    def _discard_work(self, work):
        for capability in (
            capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION,
            capability_pb2.SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION,
            capability_pb2.SERVICE_CAPABILITY_KIND_FACE_LIVENESS,
        ):
            self._publisher.discard(capability)
        self._frames.close()
        self._frames = _LatestFrame()
        self._finish_work(work)

    def _mark_identity_unavailable(self, work):
        required = set()
        if work.detection:
            required.add(capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION)
        if work.identification:
            required.add(capability_pb2.SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION)
        if work.liveness:
            required.add(capability_pb2.SERVICE_CAPABILITY_KIND_FACE_LIVENESS)
        for session in self._identity_sessions:
            if getattr(session, "capability", None) in required:
                session.mark_unhealthy("MODEL_UNAVAILABLE")

    def _now(self):
        value = self._utcnow()
        if not isinstance(value, datetime.datetime) or value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("UTC clock must return timezone-aware values")
        return value.astimezone(UTC)


def _wipe(value):
    if value is None:
        return
    if isinstance(value, bytearray):
        value[:] = bytes(len(value))
        return
    fill = getattr(value, "fill", None)
    if callable(fill):
        fill(0)
