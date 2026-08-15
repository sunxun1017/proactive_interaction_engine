"""Local PCM framing and stable voice activity; raw audio is never persisted."""

import argparse
from collections import deque
import datetime
import signal
import subprocess
import threading
import uuid

from proactive.platform.v1 import capability_pb2

from workers.provider import ProviderConfig, ProviderSession


SAMPLE_RATE = 16_000
FRAME_MS = 20
SAMPLE_WIDTH = 2
FRAME_BYTES = SAMPLE_RATE * FRAME_MS // 1_000 * SAMPLE_WIDTH


class PCMFramer:
    """Yields WebRTC-VAD frames while retaining only one incomplete tail."""

    _SUPPORTED_FRAME_MS = (10, 20, 30)
    _SUPPORTED_SAMPLE_RATES = (8_000, 16_000, 32_000, 48_000)

    def __init__(self, sample_rate, frame_ms, sample_width):
        if (
            sample_rate not in self._SUPPORTED_SAMPLE_RATES
            or frame_ms not in self._SUPPORTED_FRAME_MS
            or sample_width != 2
        ):
            raise ValueError("unsupported PCM frame shape")
        self._frame_bytes = sample_rate * frame_ms // 1_000 * sample_width
        self._tail = bytearray()

    @property
    def buffered_bytes(self):
        return len(self._tail)

    @property
    def tail(self):
        return bytes(self._tail)

    def push(self, chunk):
        if chunk:
            self._tail.extend(chunk)
        frames = []
        while len(self._tail) >= self._frame_bytes:
            frames.append(bytes(self._tail[: self._frame_bytes]))
            self._tail[: self._frame_bytes] = b"\x00" * self._frame_bytes
            del self._tail[: self._frame_bytes]
        return frames


class VoiceActivityGate:
    """Triggers on a voiced sliding window and rearms after stable silence."""

    def __init__(self, window_frames=15, voiced_frames=12, rearm_silence_frames=25):
        if (
            window_frames <= 0
            or voiced_frames <= 0
            or voiced_frames > window_frames
            or rearm_silence_frames <= 0
        ):
            raise ValueError("voice activity thresholds are invalid")
        self._window = deque(maxlen=window_frames)
        self._window_frames = window_frames
        self._voiced_frames = voiced_frames
        self._rearm_silence_frames = rearm_silence_frames
        self._armed = True
        self._silence_run = 0

    def update(self, voiced):
        voiced = bool(voiced)
        if not self._armed:
            self._silence_run = 0 if voiced else self._silence_run + 1
            if self._silence_run >= self._rearm_silence_frames:
                self._armed = True
                self._silence_run = 0
                self._window.clear()
            return False
        self._window.append(voiced)
        if len(self._window) < self._window_frames or sum(self._window) < self._voiced_frames:
            return False
        self._armed = False
        self._silence_run = 0
        self._window.clear()
        return True


class MicrophoneWorker:
    def __init__(self, vad, session):
        if vad is None or session is None:
            raise ValueError("VAD and provider session are required")
        self._vad = vad
        self._session = session
        self._framer = PCMFramer(SAMPLE_RATE, FRAME_MS, SAMPLE_WIDTH)
        self._gate = VoiceActivityGate()

    @property
    def buffered_bytes(self):
        return self._framer.buffered_bytes

    def process(self, chunk):
        active = self._session.maintain()
        frames = self._framer.push(chunk)
        for frame in frames:
            try:
                voiced = self._vad.is_speech(frame, SAMPLE_RATE)
                if self._gate.update(voiced) and active:
                    self._session.publish_speech_activity()
            finally:
                frame = None
        frames.clear()

    def run(self, source, stop_event):
        try:
            while not stop_event.is_set():
                chunk = source.read(FRAME_BYTES)
                if not chunk:
                    self._session.mark_unhealthy("DEVICE_UNAVAILABLE")
                    return
                try:
                    self.process(chunk)
                finally:
                    chunk = None
        finally:
            source.close()
            self._session.close()


def parec_command(binary):
    if not binary or binary.strip() != binary:
        raise ValueError("parec binary is required")
    return [
        binary,
        "--raw",
        "--format=s16le",
        "--rate=16000",
        "--channels=1",
    ]


class ParecSource:
    def __init__(self, binary):
        self._process = subprocess.Popen(
            parec_command(binary),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            shell=False,
            bufsize=0,
        )
        if self._process.stdout is None:
            self.close()
            raise RuntimeError("parec did not provide PCM stdout")

    def read(self, size):
        chunks = bytearray()
        while len(chunks) < size:
            part = self._process.stdout.read(size - len(chunks))
            if not part:
                break
            chunks.extend(part)
        result = bytes(chunks)
        chunks[:] = b"\x00" * len(chunks)
        return result

    def close(self):
        if self._process.poll() is None:
            self._process.terminate()
            try:
                self._process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self._process.kill()
                self._process.wait(timeout=2)
        if self._process.stdout is not None:
            self._process.stdout.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description="local hands-free VAD worker")
    parser.add_argument("--grpc-address", required=True)
    parser.add_argument("--parec", default="/usr/bin/parec")
    parser.add_argument("--provider-id", default="desktop-vad")
    parser.add_argument("--instance-id", default=f"vad-{uuid.uuid4()}")
    parser.add_argument("--subject-id", default="user-1")
    args = parser.parse_args(argv)

    import webrtcvad

    source = ParecSource(args.parec)
    try:
        session = ProviderSession.connect(
            args.grpc_address,
            ProviderConfig(
                provider_id=args.provider_id,
                instance_id=args.instance_id,
                capability=capability_pb2.SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY,
                implementation_version="webrtc-vad.v1",
                privacy_class=capability_pb2.PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
                maximum_latency=datetime.timedelta(seconds=1),
                cancellation_semantics=capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
                device_requirements=(capability_pb2.PROVIDER_DEVICE_CLASS_MICROPHONE,),
                subject_id=args.subject_id,
                observation_ttl=datetime.timedelta(seconds=1),
            ),
        )
    except Exception:
        source.close()
        raise
    session.start()
    stop_event = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop_event.set())
    signal.signal(signal.SIGINT, lambda *_: stop_event.set())
    MicrophoneWorker(webrtcvad.Vad(2), session).run(source, stop_event)


if __name__ == "__main__":
    main()
