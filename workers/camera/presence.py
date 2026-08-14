"""Anonymous person-presence detection; image data never leaves this process."""

import argparse
from collections import deque
import datetime
import signal
import threading
import uuid

from proactive.platform.v1 import capability_pb2

from workers.provider import ProviderConfig, ProviderSession


class PresenceHysteresis:
    """Enters on N-of-window detections and exits on consecutive misses."""

    def __init__(self, window_frames=5, present_hits=3, absent_frames=10):
        if (
            window_frames <= 0
            or present_hits <= 0
            or present_hits > window_frames
            or absent_frames <= 0
        ):
            raise ValueError("presence thresholds are invalid")
        self._window = deque(maxlen=window_frames)
        self._present_hits = present_hits
        self._absent_frames = absent_frames
        self._absent_run = 0
        self._current = None

    def update(self, detected):
        detected = bool(detected)
        self._window.append(detected)
        self._absent_run = 0 if detected else self._absent_run + 1
        if self._current is not True and sum(self._window) >= self._present_hits:
            self._current = True
            self._absent_run = 0
            return True
        if self._current is not False and self._absent_run >= self._absent_frames:
            self._current = False
            self._window.clear()
            return False
        return None


class CameraWorker:
    """Reads frames locally and publishes only stable boolean transitions."""

    def __init__(self, source, detector, session):
        if source is None or detector is None or session is None:
            raise ValueError("camera source, detector, and provider session are required")
        self._source = source
        self._detector = detector
        self._session = session
        self._gate = PresenceHysteresis()
        self._read_failures = 0
        self._pending = None

    def step(self):
        active = self._session.maintain()
        if active and self._pending is not None and self._session.publish_presence(self._pending):
            self._pending = None
        ok, frame = self._source.read()
        if not ok or frame is None:
            self._read_failures += 1
            if self._read_failures >= 3:
                self._session.mark_unhealthy("DEVICE_UNAVAILABLE")
                return False
            return True
        self._read_failures = 0
        try:
            transition = self._gate.update(self._detector(frame))
        finally:
            frame = None
        if transition is not None:
            if not active or not self._session.publish_presence(transition):
                self._pending = transition
        return True

    def run(self, stop_event, frames_per_second=5):
        if frames_per_second <= 0:
            raise ValueError("frame rate must be positive")
        interval = 1.0 / frames_per_second
        try:
            while not stop_event.is_set() and self.step():
                stop_event.wait(interval)
        finally:
            self._source.close()
            self._session.close()


class AnonymousPeopleDetector:
    """Uses only non-identity HOG and upper-body detectors."""

    def __init__(self, cv2_module):
        self._cv2 = cv2_module
        self._hog = cv2_module.HOGDescriptor()
        self._hog.setSVMDetector(cv2_module.HOGDescriptor_getDefaultPeopleDetector())
        cascade_path = cv2_module.data.haarcascades + "haarcascade_upperbody.xml"
        self._upper_body = cv2_module.CascadeClassifier(cascade_path)
        if self._upper_body.empty():
            raise RuntimeError("OpenCV upper-body cascade is unavailable")

    def __call__(self, frame):
        resized = self._cv2.resize(frame, (640, 480))
        people, _ = self._hog.detectMultiScale(
            resized,
            winStride=(8, 8),
            padding=(8, 8),
            scale=1.05,
        )
        if len(people) > 0:
            return True
        gray = self._cv2.cvtColor(resized, self._cv2.COLOR_BGR2GRAY)
        bodies = self._upper_body.detectMultiScale(
            gray,
            scaleFactor=1.1,
            minNeighbors=3,
            minSize=(48, 48),
        )
        return len(bodies) > 0


class OpenCVCameraSource:
    def __init__(self, cv2_module, device):
        if not device or device.strip() != device:
            raise ValueError("camera device is required")
        self._capture = cv2_module.VideoCapture(device, cv2_module.CAP_V4L2)
        self._capture.set(cv2_module.CAP_PROP_FRAME_WIDTH, 640)
        self._capture.set(cv2_module.CAP_PROP_FRAME_HEIGHT, 480)
        self._capture.set(cv2_module.CAP_PROP_FPS, 5)
        if not self._capture.isOpened():
            self._capture.release()
            raise RuntimeError(f"camera device {device!r} is unavailable")

    def read(self):
        return self._capture.read()

    def close(self):
        self._capture.release()


def main(argv=None):
    parser = argparse.ArgumentParser(description="anonymous local person-presence worker")
    parser.add_argument("--grpc-address", required=True)
    parser.add_argument("--device", default="/dev/video0")
    parser.add_argument("--provider-id", default="desktop-presence")
    parser.add_argument("--instance-id", default=f"presence-{uuid.uuid4()}")
    parser.add_argument("--subject-id", default="user-1")
    args = parser.parse_args(argv)

    import cv2

    detector = AnonymousPeopleDetector(cv2)
    source = OpenCVCameraSource(cv2, args.device)
    try:
        session = ProviderSession.connect(
            args.grpc_address,
            ProviderConfig(
                provider_id=args.provider_id,
                instance_id=args.instance_id,
                capability=capability_pb2.SERVICE_CAPABILITY_KIND_PERSON_PRESENCE,
                implementation_version="opencv-anonymous-presence.v1",
                subject_id=args.subject_id,
                observation_ttl=datetime.timedelta(seconds=2),
            ),
        )
    except Exception:
        source.close()
        raise
    session.start()
    stop_event = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop_event.set())
    signal.signal(signal.SIGINT, lambda *_: stop_event.set())
    CameraWorker(source, detector, session).run(stop_event)


if __name__ == "__main__":
    main()
