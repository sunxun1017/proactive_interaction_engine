"""Single-microphone owner combining continuous VAD and bounded identity work."""

from collections import deque
from dataclasses import dataclass
import datetime
import threading
import time

from proactive.platform.v1 import capability_pb2

from workers.identity_control import (
    EvidenceCandidate,
    LatestIdentityWork,
    VerificationCandidate,
)
from workers.microphone.activity import FRAME_BYTES, FRAME_MS, PCMFramer, SAMPLE_RATE, SAMPLE_WIDTH


UTC = datetime.timezone.utc


@dataclass(frozen=True)
class AudioInferenceResult:
    candidates: tuple = ()
    verification: VerificationCandidate = None

    def __post_init__(self):
        candidates = tuple(self.candidates)
        if any(not isinstance(item, EvidenceCandidate) for item in candidates):
            raise ValueError("audio inference candidates are invalid")
        if self.verification is not None and not isinstance(self.verification, VerificationCandidate):
            raise ValueError("audio verification candidate is invalid")
        object.__setattr__(self, "candidates", candidates)


class _BoundedPCM:
    def __init__(self, maximum_bytes):
        if type(maximum_bytes) is not int or maximum_bytes < FRAME_BYTES or maximum_bytes % FRAME_BYTES:
            raise ValueError("PCM bound must contain complete fixed frames")
        self._maximum = maximum_bytes
        self._frames = deque()
        self._bytes = 0
        self._last_occurred_at = None
        self._lock = threading.Lock()

    def append(self, frame, occurred_at):
        value = bytearray(frame)
        with self._lock:
            self._frames.append(value)
            self._bytes += len(value)
            self._last_occurred_at = occurred_at
            while self._bytes > self._maximum:
                removed = self._frames.popleft()
                self._bytes -= len(removed)
                removed[:] = bytes(len(removed))

    def snapshot(self):
        with self._lock:
            if not self._frames:
                return None, None
            return bytearray().join(self._frames), self._last_occurred_at

    @property
    def size(self):
        with self._lock:
            return self._bytes

    def clear(self):
        with self._lock:
            while self._frames:
                frame = self._frames.popleft()
                frame[:] = bytes(len(frame))
            self._bytes = 0
            self._last_occurred_at = None


class AudioStack:
    """Own one microphone; segmentation and inference are explicit injections."""

    def __init__(
        self,
        source,
        vad,
        voice_gate,
        base_session,
        identity_sessions,
        desired,
        publisher,
        inference,
        *,
        max_pcm_bytes,
        segment_ready,
        utcnow,
        join_timeout,
        control_client=None,
        reconnect_delay=None,
    ):
        if any(item is None for item in (source, vad, voice_gate, base_session, desired, publisher, inference)):
            raise ValueError("audio stack dependencies are required")
        if not isinstance(desired, LatestIdentityWork) or not callable(segment_ready) or not callable(utcnow):
            raise ValueError("audio desired state, segment rule, and clock are required")
        if join_timeout <= 0 or (control_client is None) != (reconnect_delay is None):
            raise ValueError("positive join timeout and complete control configuration are required")
        if reconnect_delay is not None and reconnect_delay <= 0:
            raise ValueError("control reconnect delay must be positive")
        self._source = source
        self._vad = vad
        self._voice_gate = voice_gate
        self._base_session = base_session
        self._identity_sessions = tuple(identity_sessions)
        self._desired = desired
        self._publisher = publisher
        self._inference = inference
        self._segment_ready = segment_ready
        self._utcnow = utcnow
        self._join_timeout = join_timeout
        self._control_client = control_client
        self._reconnect_delay = reconnect_delay
        self._framer = PCMFramer(SAMPLE_RATE, FRAME_MS, SAMPLE_WIDTH)
        self._pcm = _BoundedPCM(max_pcm_bytes)
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
    def buffered_pcm_bytes(self):
        return self._pcm.size

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
        chunk = self._source.read(FRAME_BYTES)
        if not chunk:
            return False
        occurred_at = self._now()
        frames = self._framer.push(chunk)
        try:
            for frame in frames:
                try:
                    voiced = self._vad.is_speech(frame, SAMPLE_RATE)
                    if self._voice_gate.update(voiced) and active:
                        self._base_session.publish_speech_activity()
                    work = self._pending()
                    if (
                        work is not None
                        and self._desired.is_current(work)
                        and work.opened_at <= occurred_at < work.deadline
                    ):
                        self._pcm.append(frame, occurred_at)
                finally:
                    frame = None
        finally:
            frames.clear()
            chunk = None
        self._wake.set()
        return True

    def process_identity_once(self):
        with self._pending_lock:
            if self._pending_work is None:
                self._pending_work = self._desired.claim()
                if self._pending_work is not None:
                    self._pcm.clear()
            work = self._pending_work
        if work is None:
            return False
        if not self._desired.is_current(work):
            self._discard_work(work)
            return False
        if not bool(self._segment_ready(self._pcm.size)):
            return False
        pcm, occurred_at = self._pcm.snapshot()
        if pcm is None:
            return False
        try:
            with self._inference_lock:
                result = self._inference.infer(work, bytes(pcm))
            if not isinstance(result, AudioInferenceResult):
                raise ValueError("audio inference returned an invalid result")
            if not self._desired.is_current(work) or occurred_at is None or not occurred_at < work.deadline:
                return False
            if work.kind == "identification":
                if result.verification is not None:
                    raise ValueError("speaker identification returned verification evidence")
                self._publisher.publish_speaker_identification(
                    work.evidence_window_id, occurred_at, work.deadline, result.candidates
                )
            elif work.kind == "verification":
                if result.verification is None or result.candidates:
                    raise ValueError("speaker verification returned identification evidence")
                self._publisher.publish_speaker_verification(
                    work.challenge_id,
                    work.evidence_window_id,
                    occurred_at,
                    work.deadline,
                    result.verification,
                )
            else:
                raise ValueError("audio identity task kind is unknown")
            return True
        except Exception:
            self._mark_identity_unavailable(work)
            return False
        finally:
            pcm[:] = bytes(len(pcm))
            self._pcm.clear()
            self._finish_work(work)

    def run(self, stop_event):
        if not isinstance(stop_event, threading.Event):
            raise ValueError("audio stop event is required")
        with self._close_lock:
            if self._shutdown_requested or self._run_stop_event is not None:
                raise RuntimeError("audio stack may only run once")
            self._run_stop_event = stop_event
            self._capture_done.clear()
        identity_thread = threading.Thread(
            target=self._identity_loop, args=(stop_event,), name="audio-identity", daemon=False
        )
        self._threads = [identity_thread]
        if self._control_client is not None:
            self._threads.append(threading.Thread(
                target=self._control_client.run,
                args=(stop_event, self._reconnect_delay),
                name="audio-control",
                daemon=False,
            ))
        for thread in self._threads:
            thread.start()
        self._wake.set()
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
        # Invalidate the revision without clearing executor-owned templates or
        # PCM. Final wiping happens only after the identity thread is joined.
        self._desired.close()
        self._source.close()

    def _join_owned_threads(self, *, wait_for_capture):
        if any(thread.ident == threading.get_ident() for thread in self._threads):
            raise RuntimeError("audio stack cannot join from an owned thread")
        deadline = time.monotonic() + self._join_timeout
        if wait_for_capture:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not self._capture_done.wait(remaining):
                raise RuntimeError("audio stack failed to join capture owner")
        for thread in self._threads:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            thread.join(remaining)
        if self.owned_threads_alive:
            raise RuntimeError("audio stack failed to join owned threads")

    def _finalize_shutdown(self):
        with self._close_lock:
            if self._finalized:
                return
            if self.owned_threads_alive:
                raise RuntimeError("audio executor must exit before final cleanup")
            self._finalized = True
        self._pcm.clear()
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

    def _pending(self):
        with self._pending_lock:
            return self._pending_work

    def _finish_work(self, work):
        with self._pending_lock:
            if self._pending_work is work:
                self._pending_work = None
        self._desired.release(work)

    def _discard_work(self, work):
        capability = (
            capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION
            if work.kind == "identification"
            else capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION
        )
        self._publisher.discard(capability)
        self._pcm.clear()
        self._finish_work(work)

    def _mark_identity_unavailable(self, work):
        capability = (
            capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION
            if work.kind == "identification"
            else capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION
        )
        for session in self._identity_sessions:
            if getattr(session, "capability", None) == capability:
                session.mark_unhealthy("MODEL_UNAVAILABLE")

    def _now(self):
        value = self._utcnow()
        if not isinstance(value, datetime.datetime) or value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("UTC clock must return timezone-aware values")
        return value.astimezone(UTC)
