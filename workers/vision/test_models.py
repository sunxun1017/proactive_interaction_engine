import json
from pathlib import Path
import unittest

import numpy as np

from workers.vision.models import (
    AntiSpoofClassifier,
    DetectedFace,
    LivenessScores,
    ModelUnavailableError,
    SFaceRecognizer,
    YuNetDetector,
    create_cuda_onnx_engine,
)


class CudaOnnxEngineTest(unittest.TestCase):
    def test_creates_cuda_only_session_with_both_fallback_paths_disabled(self):
        ort = FakeOrt()

        engine = create_cuda_onnx_engine(ort, "/models/model.onnx", 0)

        self.assertEqual(
            ort.providers_argument,
            [("CUDAExecutionProvider", {"device_id": 0})],
        )
        self.assertEqual(ort.events[:2], ["preload", "session"])
        self.assertEqual(ort.preload_directory, "")
        self.assertEqual(
            ort.session_options.entries["session.disable_cpu_ep_fallback"], "1"
        )
        self.assertTrue(ort.session.fallback_disabled)
        self.assertEqual(engine.execution_provider, "CUDAExecutionProvider")
        self.assertTrue(engine.cpu_fallback_disabled)
        self.assertEqual(engine.device_index, 0)

    def test_profile_audit_requires_every_executed_node_to_use_cuda(self):
        for providers, accepted in (
            (["CUDAExecutionProvider", "CUDAExecutionProvider"], True),
            (["CUDAExecutionProvider", "CPUExecutionProvider"], False),
            ([], False),
        ):
            with self.subTest(providers=providers):
                ort = FakeOrt(profile_providers=providers)
                engine = create_cuda_onnx_engine(
                    ort, "/models/model.onnx", 0, enable_profiling=True
                )
                if accepted:
                    engine.assert_cuda_only_execution()
                else:
                    with self.assertRaises(ModelUnavailableError):
                        engine.assert_cuda_only_execution()

    def test_context_cleanup_preserves_original_inference_error(self):
        ort = FakeOrt(end_profile_error=RuntimeError("profile cleanup failed"))
        engine = create_cuda_onnx_engine(
            ort, "/models/model.onnx", 0, enable_profiling=True
        )
        profile_root = Path(ort.session_options.profile_file_prefix).parent

        with self.assertRaisesRegex(ValueError, "original inference error"):
            with engine:
                raise ValueError("original inference error")

        self.assertFalse(profile_root.exists())

    def test_fails_closed_when_cuda_is_unavailable_or_not_activated(self):
        no_cuda = FakeOrt(available_providers=["CPUExecutionProvider"])
        with self.assertRaises(ModelUnavailableError):
            create_cuda_onnx_engine(no_cuda, "/models/model.onnx", 0)

        inactive = FakeOrt(active_providers=["CPUExecutionProvider"])
        with self.assertRaises(ModelUnavailableError):
            create_cuda_onnx_engine(inactive, "/models/model.onnx", 0)

        wrong_device = FakeOrt(active_device_index=1)
        with self.assertRaises(ModelUnavailableError):
            create_cuda_onnx_engine(wrong_device, "/models/model.onnx", 0)

        missing_device = FakeOrt(report_device=False)
        with self.assertRaises(ModelUnavailableError):
            create_cuda_onnx_engine(missing_device, "/models/model.onnx", 0)

        preload_failure = FakeOrt(preload_error=RuntimeError("missing CUDA runtime"))
        with self.assertRaises(ModelUnavailableError):
            create_cuda_onnx_engine(preload_failure, "/models/model.onnx", 0)
        self.assertIsNone(preload_failure.session)

    def test_requires_an_explicit_bounded_nonboolean_device_index(self):
        for device_index in (True, -1, 32, 1.5, "0"):
            with self.subTest(device_index=device_index), self.assertRaises(ValueError):
                create_cuda_onnx_engine(FakeOrt(), "/models/model.onnx", device_index)


class YuNetDetectorTest(unittest.TestCase):
    def test_decodes_strict_finite_detection_without_choosing_identity(self):
        engine = FakeCudaEngine(yunet_outputs())
        detector = YuNetDetector(
            engine,
            FakeCV2(),
            score_threshold=0.42,
            nms_threshold=0.31,
            top_k=17,
        )

        frame = np.zeros((640, 640, 3), dtype=np.uint8)
        frame[0, 0] = (10, 20, 30)
        faces = detector.detect(frame)

        self.assertEqual(engine.last_inputs["input"].shape, (1, 3, 640, 640))
        self.assertEqual(tuple(engine.last_inputs["input"][0, :, 0, 0]), (10, 20, 30))
        self.assertEqual(len(faces), 1)
        np.testing.assert_allclose(faces[0].box, (8.0, 8.0, 16.0, 16.0))
        self.assertEqual(len(faces[0].landmarks), 5)
        self.assertAlmostEqual(faces[0].score, 0.9)

    def test_rejects_invalid_thresholds_frames_and_model_output(self):
        engine = FakeCudaEngine(yunet_outputs())
        for score, nms, top_k in ((-0.1, 0.3, 1), (0.5, 1.1, 1), (0.5, 0.3, 0)):
            with self.subTest(score=score, nms=nms, top_k=top_k), self.assertRaises(ValueError):
                YuNetDetector(engine, FakeCV2(), score, nms, top_k)
        detector = YuNetDetector(engine, FakeCV2(), 0.5, 0.3, 1)
        with self.assertRaises(ValueError):
            detector.detect(np.zeros((10, 10), dtype=np.uint8))
        engine.outputs["cls_8"][0, 0, 0] = np.nan
        with self.assertRaises(ValueError):
            detector.detect(np.zeros((640, 640, 3), dtype=np.uint8))

    def test_rejects_a_cpu_or_fallback_enabled_engine(self):
        outputs = yunet_outputs()
        with self.assertRaises(ModelUnavailableError):
            YuNetDetector(
                FakeCudaEngine(outputs, provider="CPUExecutionProvider"),
                FakeCV2(),
                0.5,
                0.3,
                1,
            )
        with self.assertRaises(ModelUnavailableError):
            YuNetDetector(
                FakeCudaEngine(outputs, fallback_disabled=False),
                FakeCV2(),
                0.5,
                0.3,
                1,
            )

    def test_letterboxes_fixed_onnx_input_and_maps_geometry_to_original_frame(self):
        engine = FakeCudaEngine(yunet_outputs())
        detector = YuNetDetector(engine, FakeCV2(), 0.42, 0.31, 17)

        faces = detector.detect(np.zeros((960, 1280, 3), dtype=np.uint8))

        self.assertEqual(engine.last_inputs["input"].shape, (1, 3, 640, 640))
        np.testing.assert_allclose(faces[0].box, (16.0, 16.0, 32.0, 32.0))


class SFaceRecognizerTest(unittest.TestCase):
    def test_aligns_with_five_landmarks_and_returns_unit_embedding(self):
        cv2 = FakeCV2()
        engine = FakeCudaEngine({"fc1": np.arange(1, 129, dtype=np.float32)[None]})
        recognizer = SFaceRecognizer(engine, cv2)

        embedding = recognizer.embedding(
            np.zeros((100, 100, 3), dtype=np.uint8),
            sample_face(),
        )

        self.assertEqual(cv2.last_warp_size, (112, 112))
        self.assertEqual(engine.last_inputs["data"].shape, (1, 3, 112, 112))
        self.assertEqual(embedding.shape, (128,))
        self.assertAlmostEqual(float(np.linalg.norm(embedding)), 1.0, places=6)

    def test_sface_uses_official_raw_rgb_blob_without_extra_mean_or_scale(self):
        cv2 = FakeCV2()
        engine = FakeCudaEngine({"fc1": np.arange(1, 129, dtype=np.float32)[None]})
        recognizer = SFaceRecognizer(engine, cv2)
        aligned = np.zeros((112, 112, 3), dtype=np.uint8)
        aligned[:, :, 0] = 10
        aligned[:, :, 1] = 20
        aligned[:, :, 2] = 30

        recognizer.embedding_from_aligned(aligned)

        self.assertEqual(tuple(engine.last_inputs["data"][0, :, 0, 0]), (30, 20, 10))

    def test_aligned_embedding_rejects_wrong_output_dimension_and_nonfinite_values(self):
        cv2 = FakeCV2()
        engine = FakeCudaEngine({"fc1": np.ones((1, 127), np.float32)})
        recognizer = SFaceRecognizer(engine, cv2)
        with self.assertRaises(ValueError):
            recognizer.embedding_from_aligned(np.zeros((112, 112, 3), np.uint8))
        engine.outputs["fc1"] = np.full((1, 128), np.nan)
        with self.assertRaises(ValueError):
            recognizer.embedding_from_aligned(np.zeros((112, 112, 3), np.uint8))

    def test_similarity_is_finite_without_applying_a_match_threshold(self):
        left = np.zeros(128, np.float32)
        right = np.zeros(128, np.float32)
        left[0] = 1
        right[1] = 1

        self.assertEqual(SFaceRecognizer.cosine_similarity(left, left), 1.0)
        self.assertEqual(SFaceRecognizer.cosine_similarity(left, right), 0.0)
        with self.assertRaises(ValueError):
            SFaceRecognizer.cosine_similarity(left, np.full(128, np.nan))


class AntiSpoofClassifierTest(unittest.TestCase):
    def test_preprocesses_original_onnx_rgb_input_and_returns_scores_only(self):
        cv2 = FakeCV2()
        engine = FakeCudaEngine({"output1": np.array([[0.75, 0.25]], np.float32)})
        classifier = AntiSpoofClassifier(engine, cv2)
        crop = np.zeros((64, 64, 3), dtype=np.uint8)
        crop[:, :, 0] = 10
        crop[:, :, 1] = 20
        crop[:, :, 2] = 30

        scores = classifier.score(crop)

        self.assertEqual(scores, LivenessScores(real=0.75, spoof=0.25))
        self.assertFalse(hasattr(scores, "passed"))
        blob = engine.last_inputs["actual_input_1"]
        self.assertEqual(blob.shape, (1, 3, 128, 128))
        self.assertAlmostEqual(float(blob[0, 0, 0, 0]), (30 - 151.2405) / 63.0105, places=5)
        self.assertAlmostEqual(float(blob[0, 2, 0, 0]), (10 - 107.8395) / 55.0035, places=5)

    def test_rejects_malformed_or_nonprobability_output(self):
        cv2 = FakeCV2()
        engine = FakeCudaEngine({"output1": np.array([[0.5]], np.float32)})
        classifier = AntiSpoofClassifier(engine, cv2)
        for output in (
            np.array([[0.5]], np.float32),
            np.array([[np.nan, 0.5]], np.float32),
            np.array([[1.1, -0.1]], np.float32),
            np.array([[0.7, 0.4]], np.float32),
        ):
            engine.outputs["output1"] = output
            with self.subTest(output=output), self.assertRaises(ValueError):
                classifier.score(np.zeros((32, 32, 3), np.uint8))


class FakeCudaEngine:
    def __init__(self, outputs, provider="CUDAExecutionProvider", fallback_disabled=True):
        self.outputs = outputs
        self.execution_provider = provider
        self.cpu_fallback_disabled = fallback_disabled
        self.last_inputs = None
        self.last_output_names = None

    def run(self, output_names, inputs):
        self.last_output_names = tuple(output_names)
        self.last_inputs = inputs
        return tuple(self.outputs[name] for name in output_names)


class FakeSessionOptions:
    def __init__(self):
        self.entries = {}
        self.enable_profiling = False
        self.profile_file_prefix = None

    def add_session_config_entry(self, key, value):
        self.entries[key] = value


class FakeSession:
    def __init__(
        self,
        providers,
        profile_providers,
        active_device_index,
        report_device,
        end_profile_error,
    ):
        self.providers = providers
        self.profile_providers = profile_providers
        self.active_device_index = active_device_index
        self.report_device = report_device
        self.end_profile_error = end_profile_error
        self.fallback_disabled = False

    def get_providers(self):
        return list(self.providers)

    def get_provider_options(self):
        if not self.report_device:
            return {}
        return {
            "CUDAExecutionProvider": {"device_id": str(self.active_device_index)}
        }

    def disable_fallback(self):
        self.fallback_disabled = True

    def run(self, _output_names, _inputs):
        return ()

    def end_profiling(self):
        if self.end_profile_error is not None:
            raise self.end_profile_error
        path = Path(self.profile_prefix + ".json")
        events = [
            {"cat": "Node", "args": {"provider": provider}}
            for provider in self.profile_providers
        ]
        path.write_text(json.dumps(events), encoding="utf-8")
        return str(path)


class FakeOrt:
    def __init__(
        self,
        available_providers=None,
        active_providers=None,
        profile_providers=None,
        preload_error=None,
        active_device_index=0,
        report_device=True,
        end_profile_error=None,
    ):
        defaults = ["CUDAExecutionProvider", "CPUExecutionProvider"]
        self.available_providers = available_providers or defaults
        self.active_providers = active_providers or defaults
        self.profile_providers = profile_providers or []
        self.preload_error = preload_error
        self.active_device_index = active_device_index
        self.report_device = report_device
        self.end_profile_error = end_profile_error
        self.session_options = None
        self.providers_argument = None
        self.session = None
        self.events = []
        self.preload_directory = None

    def preload_dlls(self, directory):
        self.events.append("preload")
        self.preload_directory = directory
        if self.preload_error is not None:
            raise self.preload_error

    def get_available_providers(self):
        return list(self.available_providers)

    def SessionOptions(self):
        self.session_options = FakeSessionOptions()
        return self.session_options

    def InferenceSession(self, _model_path, sess_options, providers):
        self.events.append("session")
        self.assert_options_identity = sess_options is self.session_options
        self.providers_argument = providers
        self.session = FakeSession(
            self.active_providers,
            self.profile_providers,
            self.active_device_index,
            self.report_device,
            self.end_profile_error,
        )
        self.session.profile_prefix = sess_options.profile_file_prefix
        return self.session


class FakeCV2:
    COLOR_BGR2RGB = 7
    INTER_LINEAR = 1
    BORDER_CONSTANT = 0

    def __init__(self):
        self.last_warp_matrix = None
        self.last_warp_size = None

    @staticmethod
    def resize(image, size):
        return np.resize(image, (size[1], size[0], 3))

    @staticmethod
    def cvtColor(image, _conversion):
        return image[:, :, ::-1]

    def warpAffine(self, image, matrix, size, flags, borderMode):
        self.last_warp_matrix = matrix
        self.last_warp_size = size
        return np.resize(image, (size[1], size[0], 3))


def yunet_outputs():
    outputs = {}
    for stride, count in ((8, 6400), (16, 1600), (32, 400)):
        outputs[f"bbox_{stride}"] = np.zeros((1, count, 4), np.float32)
        outputs[f"cls_{stride}"] = np.zeros((1, count, 1), np.float32)
        outputs[f"kps_{stride}"] = np.zeros((1, count, 10), np.float32)
        outputs[f"obj_{stride}"] = np.zeros((1, count, 1), np.float32)
    index = 81  # row=1, column=1 in the 80x80 stride-8 feature map.
    outputs["bbox_8"][0, index] = (1, 1, np.log(2), np.log(2))
    outputs["cls_8"][0, index, 0] = 0.9
    outputs["obj_8"][0, index, 0] = 0.9
    outputs["kps_8"][0, index] = np.tile((1.0, 1.0), 5)
    return outputs


def sample_face():
    return DetectedFace(
        box=(10, 20, 30, 40),
        landmarks=((11, 21), (12, 22), (13, 23), (14, 24), (15, 25)),
        score=0.9,
    )
