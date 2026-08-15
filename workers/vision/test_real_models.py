import hashlib
import os
from pathlib import Path
import unittest

import numpy as np

from workers.models.download import load_manifest
from workers.vision.models import (
    AntiSpoofClassifier,
    SFaceRecognizer,
    YuNetDetector,
    _prepare_yunet_image,
    create_cuda_onnx_engine,
)
from workers.vision.template import normalize_embedding


@unittest.skipUnless(os.environ.get("PIE_REAL_MODEL_DIR"), "PIE_REAL_MODEL_DIR is not set")
class RealModelCudaConformanceTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = Path(os.environ["PIE_REAL_MODEL_DIR"])
        cls.manifest = load_manifest(
            Path(__file__).parents[1] / "models" / "manifest.v1.json"
        )

    def test_artifacts_match_manifest(self):
        visual_ids = {
            "opencv-yunet-2023mar",
            "opencv-sface-2021dec",
            "omz-anti-spoof-mn3",
        }
        models = tuple(
            model for model in self.manifest.models if model.model_id in visual_ids
        )
        self.assertEqual({model.model_id for model in models}, visual_ids)
        for model in models:
            with self.subTest(model=model.model_id):
                path = self.root / model.filename
                self.assertEqual(path.stat().st_size, model.size_bytes)
                self.assertEqual(_sha256(path), model.sha256)

    def test_yunet_runs_on_cuda_and_reports_no_face_for_blank_frame(self):
        cv2, ort = self._runtime()
        engine = self._engine(ort, "face_detection_yunet_2023mar.onnx")
        with engine:
            self.assertEqual(engine.input_shape("input"), (1, 3, 640, 640))
            detector = YuNetDetector(
                engine,
                cv2,
                score_threshold=0.5,
                nms_threshold=0.3,
                top_k=32,
            )
            self.assertEqual(detector.detect(np.zeros((480, 640, 3), np.uint8)), ())
            engine.assert_cuda_only_execution()

    def test_sface_runs_on_cuda_and_emits_fixed_finite_unit_embedding(self):
        cv2, ort = self._runtime()
        engine = self._engine(ort, "face_recognition_sface_2021dec.onnx")
        with engine:
            recognizer = SFaceRecognizer(
                engine,
                cv2,
            )
            embedding = recognizer.embedding_from_aligned(
                np.zeros((112, 112, 3), np.uint8)
            )
            self.assertEqual(embedding.shape, (128,))
            self.assertTrue(np.isfinite(embedding).all())
            self.assertAlmostEqual(float(np.linalg.norm(embedding)), 1.0, places=5)
            engine.assert_cuda_only_execution()

    def test_anti_spoof_runs_on_cuda_and_emits_two_finite_probabilities(self):
        cv2, ort = self._runtime()
        engine = self._engine(ort, "anti-spoof-mn3.onnx")
        with engine:
            classifier = AntiSpoofClassifier(
                engine,
                cv2,
            )
            scores = classifier.score(np.zeros((128, 128, 3), np.uint8))
            self.assertGreaterEqual(scores.real, 0)
            self.assertLessEqual(scores.real, 1)
            self.assertGreaterEqual(scores.spoof, 0)
            self.assertLessEqual(scores.spoof, 1)
            self.assertAlmostEqual(scores.real + scores.spoof, 1.0, places=5)
            engine.assert_cuda_only_execution()

    def _engine(self, ort, filename):
        return create_cuda_onnx_engine(
            ort, str(self.root / filename), 0, enable_profiling=True
        )

    def _runtime(self):
        try:
            import cv2
            import onnxruntime as ort
        except ImportError as error:
            self.fail(
                "real-model conformance requires OpenCV preprocessing and onnxruntime-gpu: "
                f"{error}"
            )
        return cv2, ort


@unittest.skipUnless(
    os.environ.get("PIE_REAL_MODEL_DIR") and os.environ.get("PIE_REAL_FACE_FIXTURE_DIR"),
    "PIE_REAL_MODEL_DIR and PIE_REAL_FACE_FIXTURE_DIR are not set",
)
class RealFaceRelationshipConformanceTest(unittest.TestCase):
    def test_same_person_similarity_exceeds_different_person_without_match_threshold(self):
        try:
            import cv2
            import onnxruntime as ort
        except ImportError as error:
            self.fail(f"real-face conformance requires the GPU media environment: {error}")

        model_root = Path(os.environ["PIE_REAL_MODEL_DIR"])
        fixture_root = Path(os.environ["PIE_REAL_FACE_FIXTURE_DIR"])
        detector_engine = create_cuda_onnx_engine(
            ort,
            str(model_root / "face_detection_yunet_2023mar.onnx"),
            0,
            enable_profiling=True,
        )
        recognizer_engine = create_cuda_onnx_engine(
            ort,
            str(model_root / "face_recognition_sface_2021dec.onnx"),
            0,
            enable_profiling=True,
        )
        detector = YuNetDetector(detector_engine, cv2, 0.5, 0.3, 32)
        recognizer = SFaceRecognizer(recognizer_engine, cv2)
        reference_detector = cv2.FaceDetectorYN.create(
            str(model_root / "face_detection_yunet_2023mar.onnx"),
            "",
            (640, 640),
            0.5,
            0.3,
            32,
            cv2.dnn.DNN_BACKEND_OPENCV,
            cv2.dnn.DNN_TARGET_CPU,
        )
        reference_recognizer = cv2.FaceRecognizerSF.create(
            str(model_root / "face_recognition_sface_2021dec.onnx"),
            "",
            cv2.dnn.DNN_BACKEND_OPENCV,
            cv2.dnn.DNN_TARGET_CPU,
        )

        embeddings = []
        for filename in ("same-person-1.jpg", "same-person-2.jpg", "different-person.jpg"):
            image = cv2.imread(str(fixture_root / filename), cv2.IMREAD_COLOR)
            self.assertIsNotNone(image, f"cannot read conformance fixture {filename}")
            faces = detector.detect(image)
            self.assertEqual(len(faces), 1, f"fixture {filename} must contain exactly one face")
            prepared, map_x, map_y = _prepare_yunet_image(image, cv2)
            reference_detector.setInputSize((640, 640))
            _, reference_faces = reference_detector.detect(prepared)
            self.assertIsNotNone(reference_faces)
            self.assertEqual(len(reference_faces), 1)
            reference_face = reference_faces[0].copy()
            reference_face[[0, 2, 4, 6, 8, 10, 12]] *= map_x
            reference_face[[1, 3, 5, 7, 9, 11, 13]] *= map_y
            gpu_face = np.asarray(
                tuple(faces[0].model_row()) + (faces[0].score,), dtype=np.float32
            )
            np.testing.assert_allclose(gpu_face, reference_face, rtol=1e-4, atol=1e-3)

            embedding = recognizer.embedding(image, faces[0])
            reference_aligned = reference_recognizer.alignCrop(image, reference_face)
            reference_embedding = normalize_embedding(
                reference_recognizer.feature(reference_aligned).reshape(-1)
            )
            self.assertGreater(
                recognizer.cosine_similarity(embedding, reference_embedding), 0.9999
            )
            embeddings.append(embedding)

        same_score = recognizer.cosine_similarity(embeddings[0], embeddings[1])
        different_score = recognizer.cosine_similarity(embeddings[0], embeddings[2])
        self.assertGreater(same_score, different_score)
        detector_engine.assert_cuda_only_execution()
        recognizer_engine.assert_cuda_only_execution()


def _sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()
