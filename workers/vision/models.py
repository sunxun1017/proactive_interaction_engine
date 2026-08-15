"""Product-policy-free CUDA ONNX wrappers for pinned visual identity models.

OpenCV is used only for image preprocessing and geometric alignment. Model
inference is delegated to an injected engine which must explicitly attest that
CUDA is active and CPU fallback is disabled.
"""

from dataclasses import dataclass
import json
import math
from pathlib import Path
import tempfile

import numpy as np

from workers.vision.template import FACE_TEMPLATE_DIMENSION, normalize_embedding


CUDA_EXECUTION_PROVIDER = "CUDAExecutionProvider"


class ModelUnavailableError(RuntimeError):
    """The requested model cannot run under the required GPU policy."""


class CudaOnnxEngine:
    """Small adapter over a CUDA-only ONNX Runtime session."""

    execution_provider = CUDA_EXECUTION_PROVIDER
    cpu_fallback_disabled = True

    def __init__(self, session, device_index, profile_directory=None):
        self._session = session
        self.device_index = device_index
        self._profile_directory = profile_directory

    def __enter__(self):
        return self

    def __exit__(self, error_type, _error, _traceback):
        try:
            self.close()
        except Exception:
            if error_type is None:
                raise
        return False

    def run(self, output_names, inputs):
        return tuple(self._session.run(list(output_names), inputs))

    def input_shape(self, input_name):
        if not isinstance(input_name, str) or not input_name:
            raise ValueError("model input name must be a non-empty string")
        try:
            matches = [item for item in self._session.get_inputs() if item.name == input_name]
        except Exception as error:
            raise ModelUnavailableError("cannot inspect ONNX model inputs") from error
        if len(matches) != 1:
            raise ModelUnavailableError(f"ONNX model input {input_name!r} is unavailable")
        return tuple(matches[0].shape)

    def close(self):
        """End profiling and release its temporary files, if profiling is active."""

        if self._profile_directory is None:
            return
        profile_directory = self._profile_directory
        self._profile_directory = None
        try:
            self._session.end_profiling()
        except Exception as error:
            raise ModelUnavailableError("cannot close ONNX Runtime profiling") from error
        finally:
            profile_directory.cleanup()

    def assert_cuda_only_execution(self):
        """End an enabled ORT profile and reject any CPU-assigned model node."""

        if self._profile_directory is None:
            raise ModelUnavailableError("CUDA node profiling was not enabled")
        profile_root = Path(self._profile_directory.name).resolve()
        try:
            profile_path = Path(self._session.end_profiling()).resolve()
            if profile_path.parent != profile_root:
                raise ModelUnavailableError("ONNX Runtime profile escaped its temporary directory")
            with profile_path.open("r", encoding="utf-8") as file:
                events = json.load(file)
            if not isinstance(events, list):
                raise ModelUnavailableError("ONNX Runtime returned an invalid execution profile")
            node_providers = []
            for event in events:
                if not isinstance(event, dict) or event.get("cat") != "Node":
                    continue
                arguments = event.get("args")
                if isinstance(arguments, dict) and isinstance(arguments.get("provider"), str):
                    node_providers.append(arguments["provider"])
            if not node_providers:
                raise ModelUnavailableError("ONNX Runtime profile contains no executed model nodes")
            if any(provider != CUDA_EXECUTION_PROVIDER for provider in node_providers):
                raise ModelUnavailableError("ONNX Runtime assigned a model node outside CUDA")
        except ModelUnavailableError:
            raise
        except (OSError, ValueError, json.JSONDecodeError) as error:
            raise ModelUnavailableError("cannot audit ONNX Runtime node assignment") from error
        finally:
            self._profile_directory.cleanup()
            self._profile_directory = None


def create_cuda_onnx_engine(
    ort_module, model_path, device_index, *, enable_profiling=False
):
    """Create an ONNX Runtime engine that fails instead of falling back to CPU."""

    _validate_model_path(model_path)
    if type(device_index) is not int or not 0 <= device_index <= 31:
        raise ValueError("CUDA device index must be an integer within [0, 31]")
    if type(enable_profiling) is not bool:
        raise ValueError("enable_profiling must be boolean")
    if ort_module is None:
        raise ModelUnavailableError("onnxruntime-gpu is required")
    try:
        # ORT >=1.21 resolves NVIDIA CUDA/cuDNN wheels from site-packages when
        # directory is the empty string. This keeps PyTorch import ordering out
        # of the visual model boundary.
        ort_module.preload_dlls(directory="")
    except Exception as error:
        raise ModelUnavailableError("cannot preload ONNX Runtime CUDA libraries") from error
    try:
        available = tuple(ort_module.get_available_providers())
    except Exception as error:
        raise ModelUnavailableError("cannot inspect ONNX Runtime providers") from error
    if CUDA_EXECUTION_PROVIDER not in available:
        raise ModelUnavailableError("ONNX Runtime CUDA execution provider is unavailable")

    profile_directory = None
    try:
        options = ort_module.SessionOptions()
        options.add_session_config_entry("session.disable_cpu_ep_fallback", "1")
        if enable_profiling:
            profile_directory = tempfile.TemporaryDirectory(prefix="pie-ort-profile-")
            options.enable_profiling = True
            options.profile_file_prefix = str(Path(profile_directory.name) / "ort-profile")
        session = ort_module.InferenceSession(
            model_path,
            sess_options=options,
            providers=[(CUDA_EXECUTION_PROVIDER, {"device_id": device_index})],
        )
        # This disables the Python API's retry-on-CPU behavior after an EP error.
        session.disable_fallback()
        active = tuple(session.get_providers())
        provider_options = session.get_provider_options()
    except Exception as error:
        if profile_directory is not None:
            profile_directory.cleanup()
        raise ModelUnavailableError("cannot create a CUDA-only ONNX Runtime session") from error
    if not active or active[0] != CUDA_EXECUTION_PROVIDER:
        if profile_directory is not None:
            profile_directory.cleanup()
        raise ModelUnavailableError("ONNX Runtime did not activate CUDA for the model")
    cuda_options = provider_options.get(CUDA_EXECUTION_PROVIDER)
    try:
        active_device_index = int(cuda_options["device_id"])
    except (KeyError, TypeError, ValueError) as error:
        if profile_directory is not None:
            profile_directory.cleanup()
        raise ModelUnavailableError("ONNX Runtime did not report its CUDA device") from error
    if active_device_index != device_index:
        if profile_directory is not None:
            profile_directory.cleanup()
        raise ModelUnavailableError("ONNX Runtime activated the wrong CUDA device")
    return CudaOnnxEngine(session, device_index, profile_directory)


@dataclass(frozen=True)
class DetectedFace:
    box: tuple[float, float, float, float]
    landmarks: tuple[tuple[float, float], ...]
    score: float

    def model_row(self):
        if len(self.box) != 4 or len(self.landmarks) != 5:
            raise ValueError("detected face must contain one box and five landmarks")
        values = tuple(self.box) + tuple(value for point in self.landmarks for value in point)
        row = np.asarray(values, dtype=np.float32)
        if row.shape != (14,) or not np.isfinite(row).all() or row[2] <= 0 or row[3] <= 0:
            raise ValueError("detected face geometry is invalid")
        return row


@dataclass(frozen=True)
class LivenessScores:
    real: float
    spoof: float


class YuNetDetector:
    """Decode YuNet geometry and confidence without product identity policy."""

    _STRIDES = (8, 16, 32)
    _INPUT_SIZE = 640
    _OUTPUT_NAMES = tuple(
        f"{kind}_{stride}"
        for stride in _STRIDES
        for kind in ("bbox", "cls", "kps", "obj")
    )

    def __init__(self, engine, cv2_module, score_threshold, nms_threshold, top_k):
        _validate_cuda_engine(engine)
        if cv2_module is None:
            raise ValueError("OpenCV preprocessing runtime is required")
        _validate_probability(score_threshold, "YuNet score threshold")
        _validate_probability(nms_threshold, "YuNet NMS threshold")
        if type(top_k) is not int or top_k <= 0:
            raise ValueError("YuNet top_k must be positive")
        self._engine = engine
        self._cv2 = cv2_module
        self._score_threshold = float(score_threshold)
        self._nms_threshold = float(nms_threshold)
        self._top_k = top_k

    def detect(self, frame):
        image = _validate_bgr_image(frame, "YuNet frame")
        padded, map_x, map_y = _prepare_yunet_image(image, self._cv2)
        # OpenCV 4.10 FaceDetectorYN uses blobFromImage defaults: raw BGR.
        blob = np.ascontiguousarray(padded.transpose(2, 0, 1)[None], dtype=np.float32)
        outputs = self._engine.run(self._OUTPUT_NAMES, {"input": blob})
        if not isinstance(outputs, (tuple, list)) or len(outputs) != len(self._OUTPUT_NAMES):
            raise ValueError("YuNet returned an invalid output set")
        named = dict(zip(self._OUTPUT_NAMES, outputs))

        candidates = []
        ordinal = 0
        for stride in self._STRIDES:
            feature_height = self._INPUT_SIZE // stride
            feature_width = self._INPUT_SIZE // stride
            count = feature_height * feature_width
            bbox = _validated_output(named[f"bbox_{stride}"], (1, count, 4), f"bbox_{stride}")[0]
            cls = _validated_output(named[f"cls_{stride}"], (1, count, 1), f"cls_{stride}")[0, :, 0]
            kps = _validated_output(named[f"kps_{stride}"], (1, count, 10), f"kps_{stride}")[0]
            obj = _validated_output(named[f"obj_{stride}"], (1, count, 1), f"obj_{stride}")[0, :, 0]
            if np.any(cls < 0) or np.any(cls > 1) or np.any(obj < 0) or np.any(obj > 1):
                raise ValueError("YuNet classification outputs must be probabilities")

            scores = np.sqrt(cls.astype(np.float64) * obj.astype(np.float64))
            for index in np.flatnonzero(scores >= self._score_threshold):
                row, column = divmod(int(index), feature_width)
                center_x = (column + float(bbox[index, 0])) * stride
                center_y = (row + float(bbox[index, 1])) * stride
                try:
                    box_width = math.exp(float(bbox[index, 2])) * stride
                    box_height = math.exp(float(bbox[index, 3])) * stride
                except OverflowError as error:
                    raise ValueError("YuNet output contains an invalid face box") from error
                x1 = center_x - box_width / 2.0
                y1 = center_y - box_height / 2.0
                if not all(math.isfinite(value) for value in (x1, y1, box_width, box_height)):
                    raise ValueError("YuNet output contains an invalid face box")
                landmarks = tuple(
                    (
                        (column + float(kps[index, point * 2])) * stride,
                        (row + float(kps[index, point * 2 + 1])) * stride,
                    )
                    for point in range(5)
                )
                candidate = DetectedFace(
                    box=(x1, y1, box_width, box_height),
                    landmarks=landmarks,
                    score=float(scores[index]),
                )
                candidates.append((ordinal, candidate))
                ordinal += 1
        selected = _non_maximum_suppression(candidates, self._nms_threshold, self._top_k)
        return tuple(_map_detected_face(face, map_x, map_y) for face in selected)


class SFaceRecognizer:
    """Extract normalized SFace embeddings and raw cosine similarity only."""

    _REFERENCE_LANDMARKS = np.asarray(
        (
            (38.2946, 51.6963),
            (73.5318, 51.5014),
            (56.0252, 71.7366),
            (41.5493, 92.3655),
            (70.7299, 92.2041),
        ),
        dtype=np.float64,
    )

    def __init__(self, engine, cv2_module):
        _validate_cuda_engine(engine)
        if cv2_module is None:
            raise ValueError("OpenCV preprocessing runtime is required")
        self._engine = engine
        self._cv2 = cv2_module

    def embedding(self, frame, face):
        image = _validate_bgr_image(frame, "SFace frame")
        if not isinstance(face, DetectedFace):
            raise ValueError("SFace requires one detected face")
        source = np.asarray(face.landmarks, dtype=np.float64)
        transform = _similarity_transform(source, self._REFERENCE_LANDMARKS)
        aligned = self._cv2.warpAffine(
            image,
            transform,
            (112, 112),
            flags=self._cv2.INTER_LINEAR,
            borderMode=self._cv2.BORDER_CONSTANT,
        )
        return self.embedding_from_aligned(aligned)

    def embedding_from_aligned(self, aligned_face):
        image = _validate_bgr_image(aligned_face, "aligned SFace crop")
        if image.shape[:2] != (112, 112):
            image = self._cv2.resize(image, (112, 112))
        rgb = self._cv2.cvtColor(image, self._cv2.COLOR_BGR2RGB)
        # OpenCV 4.10 FaceRecognizerSF uses scale=1, mean=0, swapRB=true.
        blob = np.ascontiguousarray(rgb.transpose(2, 0, 1)[None], dtype=np.float32)
        outputs = self._engine.run(("fc1",), {"data": blob})
        raw = _single_output(outputs, "SFace").reshape(-1)
        if raw.shape != (FACE_TEMPLATE_DIMENSION,):
            raise ValueError(f"SFace output must have dimension {FACE_TEMPLATE_DIMENSION}")
        return normalize_embedding(raw)

    @staticmethod
    def cosine_similarity(left, right):
        first = _validated_unit_embedding(left)
        second = _validated_unit_embedding(right)
        similarity = float(np.dot(first.astype(np.float64), second.astype(np.float64)))
        if not math.isfinite(similarity) or similarity < -1.000001 or similarity > 1.000001:
            raise ValueError("SFace cosine similarity is invalid")
        return min(1.0, max(-1.0, similarity))


class AntiSpoofClassifier:
    """Return anti-spoof probabilities without deciding liveness state."""

    _MEAN_RGB = np.asarray((151.2405, 119.5950, 107.8395), dtype=np.float32)
    _SCALE_RGB = np.asarray((63.0105, 56.4570, 55.0035), dtype=np.float32)

    def __init__(self, engine, cv2_module):
        _validate_cuda_engine(engine)
        if cv2_module is None:
            raise ValueError("OpenCV preprocessing runtime is required")
        self._engine = engine
        self._cv2 = cv2_module

    def score(self, face_crop):
        image = _validate_bgr_image(face_crop, "anti-spoof face crop")
        resized = self._cv2.resize(image, (128, 128))
        rgb = self._cv2.cvtColor(resized, self._cv2.COLOR_BGR2RGB).astype(np.float32)
        normalized = (rgb - self._MEAN_RGB) / self._SCALE_RGB
        blob = np.ascontiguousarray(normalized.transpose(2, 0, 1)[None], dtype=np.float32)
        outputs = self._engine.run(("output1",), {"actual_input_1": blob})
        output = _single_output(outputs, "anti-spoof").astype(np.float64, copy=False).reshape(-1)
        if output.shape != (2,) or not np.isfinite(output).all():
            raise ValueError("anti-spoof output must contain two finite probabilities")
        if np.any(output < 0) or np.any(output > 1):
            raise ValueError("anti-spoof probabilities must be within [0, 1]")
        if not math.isclose(float(output.sum()), 1.0, rel_tol=0.0, abs_tol=1e-4):
            raise ValueError("anti-spoof probabilities must sum to one")
        return LivenessScores(real=float(output[0]), spoof=float(output[1]))


def _validate_model_path(model_path):
    if not isinstance(model_path, str) or not model_path or model_path.strip() != model_path:
        raise ValueError("model path must be a non-empty trimmed string")


def _validate_cuda_engine(engine):
    if (
        engine is None
        or getattr(engine, "execution_provider", None) != CUDA_EXECUTION_PROVIDER
        or getattr(engine, "cpu_fallback_disabled", None) is not True
        or not callable(getattr(engine, "run", None))
    ):
        raise ModelUnavailableError("model inference requires CUDA with CPU fallback disabled")


def _validate_bgr_image(value, label):
    if not isinstance(value, np.ndarray):
        raise ValueError(f"{label} must be a numpy array")
    if value.ndim != 3 or value.shape[0] <= 0 or value.shape[1] <= 0 or value.shape[2] != 3:
        raise ValueError(f"{label} must be a non-empty HWC BGR image")
    if value.dtype != np.uint8:
        raise ValueError(f"{label} must use uint8 pixels")
    return value


def _validate_probability(value, label):
    if isinstance(value, bool) or not isinstance(value, (int, float, np.floating)):
        raise ValueError(f"{label} must be numeric")
    numeric = float(value)
    if not math.isfinite(numeric) or numeric < 0 or numeric > 1:
        raise ValueError(f"{label} must be finite and within [0, 1]")


def _validated_unit_embedding(value):
    vector = np.asarray(value, dtype=np.float32)
    if vector.shape != (FACE_TEMPLATE_DIMENSION,) or not np.isfinite(vector).all():
        raise ValueError("SFace embedding is invalid")
    norm = float(np.linalg.norm(vector.astype(np.float64)))
    if not math.isclose(norm, 1.0, rel_tol=0.0, abs_tol=1e-4):
        raise ValueError("SFace embedding must be L2-unit normalized")
    return vector


def _validated_output(value, shape, name):
    output = np.asarray(value, dtype=np.float32)
    if output.shape != shape or not np.isfinite(output).all():
        raise ValueError(f"YuNet output {name} must have shape {shape} and finite values")
    return output


def _single_output(outputs, model_name):
    if not isinstance(outputs, (tuple, list)) or len(outputs) != 1:
        raise ValueError(f"{model_name} returned an invalid output set")
    return np.asarray(outputs[0], dtype=np.float32)


def _prepare_yunet_image(image, cv2_module):
    height, width = image.shape[:2]
    scale = min(YuNetDetector._INPUT_SIZE / width, YuNetDetector._INPUT_SIZE / height)
    resized_width = max(1, min(YuNetDetector._INPUT_SIZE, int(round(width * scale))))
    resized_height = max(1, min(YuNetDetector._INPUT_SIZE, int(round(height * scale))))
    if (resized_width, resized_height) == (width, height):
        resized = image
    else:
        resized = cv2_module.resize(image, (resized_width, resized_height))
    padded = np.zeros(
        (YuNetDetector._INPUT_SIZE, YuNetDetector._INPUT_SIZE, 3), dtype=np.uint8
    )
    padded[:resized_height, :resized_width] = resized
    return padded, width / resized_width, height / resized_height


def _map_detected_face(face, map_x, map_y):
    return DetectedFace(
        box=(
            face.box[0] * map_x,
            face.box[1] * map_y,
            face.box[2] * map_x,
            face.box[3] * map_y,
        ),
        landmarks=tuple((x * map_x, y * map_y) for x, y in face.landmarks),
        score=face.score,
    )


def _non_maximum_suppression(candidates, threshold, top_k):
    ranked = sorted(candidates, key=lambda item: (-item[1].score, item[0]))[:top_k]
    accepted = []
    for _, candidate in ranked:
        overlaps = (
            _intersection_over_union(_integer_box(candidate.box), _integer_box(other.box))
            for other in accepted
        )
        if all(overlap < threshold for overlap in overlaps):
            accepted.append(candidate)
    return tuple(accepted)


def _intersection_over_union(left, right):
    left_x2 = left[0] + left[2]
    left_y2 = left[1] + left[3]
    right_x2 = right[0] + right[2]
    right_y2 = right[1] + right[3]
    width = max(0.0, min(left_x2, right_x2) - max(left[0], right[0]))
    height = max(0.0, min(left_y2, right_y2) - max(left[1], right[1]))
    intersection = width * height
    union = left[2] * left[3] + right[2] * right[3] - intersection
    return intersection / union if union > 0 else 0.0


def _integer_box(box):
    return tuple(float(int(value)) for value in box)


def _similarity_transform(source, target):
    if source.shape != (5, 2) or target.shape != (5, 2):
        raise ValueError("face alignment requires five source and target landmarks")
    if not np.isfinite(source).all() or not np.isfinite(target).all():
        raise ValueError("face alignment landmarks must be finite")
    system = np.empty((10, 4), dtype=np.float64)
    expected = np.empty(10, dtype=np.float64)
    for index, ((x, y), (target_x, target_y)) in enumerate(zip(source, target)):
        system[index * 2] = (x, -y, 1, 0)
        system[index * 2 + 1] = (y, x, 0, 1)
        expected[index * 2] = target_x
        expected[index * 2 + 1] = target_y
    solution, _, rank, _ = np.linalg.lstsq(system, expected, rcond=None)
    if rank != 4 or not np.isfinite(solution).all():
        raise ValueError("face landmarks do not define a stable similarity transform")
    scale_cosine, scale_sine, translate_x, translate_y = solution
    return np.asarray(
        (
            (scale_cosine, -scale_sine, translate_x),
            (scale_sine, scale_cosine, translate_y),
        ),
        dtype=np.float64,
    )
