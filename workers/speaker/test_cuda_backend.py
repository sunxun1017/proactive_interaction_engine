import pathlib
import tempfile
import unittest

from workers.speaker.cuda_backend import (
    EXPECTED_WEIGHT_BYTES,
    EXPECTED_WEIGHT_SHA256,
    WEIGHT_FILENAME,
    ModelUnavailableError,
    load_checkpoint_state,
    parse_cuda_device,
    require_cuda_device,
    verify_checkpoint,
)
from workers.models.download import load_manifest


class CudaBackendValidationTest(unittest.TestCase):
    def test_runtime_checkpoint_pin_matches_shared_manifest(self):
        manifest = load_manifest(
            pathlib.Path(__file__).parents[1] / "models" / "manifest.v1.json"
        )
        matching = tuple(
            model
            for model in manifest.models
            if model.model_id == "modelscope-eres2net-speaker-v1.0.5"
        )
        self.assertEqual(len(matching), 1)
        self.assertEqual(
            (matching[0].filename, matching[0].size_bytes, matching[0].sha256),
            (WEIGHT_FILENAME, EXPECTED_WEIGHT_BYTES, EXPECTED_WEIGHT_SHA256),
        )

    def test_requires_explicit_indexed_cuda_device(self):
        self.assertEqual(parse_cuda_device("cuda:0"), 0)
        self.assertEqual(parse_cuda_device("cuda:12"), 12)
        for value in ("", "cuda", "cpu", "cuda:-1", "CUDA:0", "cuda: 0"):
            with self.subTest(value=value):
                with self.assertRaises(ValueError):
                    parse_cuda_device(value)

    def test_cuda_unavailable_or_out_of_range_does_not_fall_back(self):
        with self.assertRaises(ModelUnavailableError):
            require_cuda_device("cuda:0", FakeCuda(available=False, count=0))
        with self.assertRaises(ModelUnavailableError):
            require_cuda_device("cuda:1", FakeCuda(available=True, count=1))

        self.assertEqual(require_cuda_device("cuda:0", FakeCuda(available=True, count=1)), 0)

    def test_checkpoint_requires_regular_file_exact_size_and_hash(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "checkpoint.ckpt"
            path.write_bytes(b"trusted model")

            import hashlib

            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            verify_checkpoint(path, expected_size=13, expected_sha256=digest)
            with self.assertRaises(ModelUnavailableError):
                verify_checkpoint(path, expected_size=12, expected_sha256=digest)
            with self.assertRaises(ModelUnavailableError):
                verify_checkpoint(path, expected_size=13, expected_sha256=EXPECTED_WEIGHT_SHA256)
            with self.assertRaises(ModelUnavailableError):
                verify_checkpoint(path.with_name("missing.ckpt"), expected_size=13, expected_sha256=digest)

    def test_checkpoint_loading_is_restricted_and_has_no_pickle_fallback(self):
        runtime = FakeTorch(result={"weight": "tensor"})

        self.assertEqual(load_checkpoint_state(runtime, pathlib.Path("model.ckpt")), {"weight": "tensor"})
        self.assertEqual(
            runtime.calls,
            [(pathlib.Path("model.ckpt"), {"map_location": "cpu", "weights_only": True})],
        )

        failing = FakeTorch(error=TypeError("weights_only unsupported"))
        with self.assertRaises(ModelUnavailableError):
            load_checkpoint_state(failing, pathlib.Path("model.ckpt"))
        self.assertEqual(len(failing.calls), 1)


class FakeCuda:
    def __init__(self, available, count):
        self._available = available
        self._count = count

    def is_available(self):
        return self._available

    def device_count(self):
        return self._count


class FakeTorch:
    def __init__(self, result=None, error=None):
        self._result = result
        self._error = error
        self.calls = []

    def load(self, path, **kwargs):
        self.calls.append((path, kwargs))
        if self._error is not None:
            raise self._error
        return self._result


if __name__ == "__main__":
    unittest.main()
