import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock
from urllib.request import Request

from workers.models.download import (
    DownloadError,
    _HTTPSRedirectHandler,
    download_models,
    load_manifest,
)


class ManifestTest(unittest.TestCase):
    def test_checked_in_manifest_pins_the_reviewed_artifacts(self):
        manifest = load_manifest(Path(__file__).with_name("manifest.v1.json"))

        self.assertEqual(manifest.schema_version, 1)
        self.assertEqual(
            {
                model.model_id: (model.size_bytes, model.sha256)
                for model in manifest.models
            },
            {
                "opencv-yunet-2023mar": (
                    232589,
                    "8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4",
                ),
                "opencv-sface-2021dec": (
                    38696353,
                    "0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79",
                ),
                "omz-anti-spoof-mn3": (
                    12270179,
                    "c4c99af04603b62d7e44f6f4daeb33e0daeccc696008c0b1d62f6f5cebbb3262",
                ),
                "modelscope-eres2net-speaker-v1.0.5": (
                    221210095,
                    "ad78a02cab9dc23385c3fe235d1c3a370c0bd89ef192d0250546b370b0b8f227",
                ),
            },
        )
        for model in manifest.models:
            self.assertEqual(model.url.partition(":")[0], "https")
            self.assertNotIn("main", model.url)
            self.assertNotIn("master", model.url)

    def test_manifest_rejects_unknown_fields_and_unsafe_sources(self):
        valid = _manifest_entry(b"model")
        cases = {
            "unknown root field": {"schema_version": 1, "models": [valid], "extra": 1},
            "unknown model field": {
                "schema_version": 1,
                "models": [{**valid, "extra": 1}],
            },
            "non-https URL": {
                "schema_version": 1,
                "models": [{**valid, "url": "http://huggingface.co/model.onnx"}],
            },
            "unapproved host": {
                "schema_version": 1,
                "models": [{**valid, "url": "https://example.com/model.onnx"}],
            },
            "path filename": {
                "schema_version": 1,
                "models": [{**valid, "filename": "../model.onnx"}],
            },
        }
        for name, content in cases.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                path = Path(directory, "manifest.json")
                path.write_text(json.dumps(content), encoding="utf-8")
                with self.assertRaises(ValueError):
                    load_manifest(path)

    def test_manifest_rejects_duplicate_ids_and_filenames(self):
        entry = _manifest_entry(b"model")
        for duplicate in (
            {**entry, "filename": "second.onnx"},
            {**entry, "id": "second-model"},
        ):
            with self.subTest(duplicate=duplicate), tempfile.TemporaryDirectory() as directory:
                path = Path(directory, "manifest.json")
                path.write_text(
                    json.dumps({"schema_version": 1, "models": [entry, duplicate]}),
                    encoding="utf-8",
                )
                with self.assertRaises(ValueError):
                    load_manifest(path)

    def test_manifest_rejects_duplicate_json_fields_and_nonstandard_numbers(self):
        contents = (
            '{"schema_version":1,"schema_version":1,"models":[]}',
            '{"schema_version":1,"models":[{"id":NaN}]}',
        )
        for content in contents:
            with self.subTest(content=content), tempfile.TemporaryDirectory() as directory:
                path = Path(directory, "manifest.json")
                path.write_text(content, encoding="utf-8")
                with self.assertRaises(ValueError):
                    load_manifest(path)


class DownloaderTest(unittest.TestCase):
    def test_downloads_streams_verifies_and_reuses_exact_artifact(self):
        payload = b"reviewed model bytes"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest_path = _write_manifest(root, payload)
            opener = RecordingOpener(payload, chunk_size=3)

            first = download_models(manifest_path, root / "cache", opener=opener)
            second = download_models(manifest_path, root / "cache", opener=opener)

            target = root / "cache" / "model.onnx"
            self.assertEqual(first, (target,))
            self.assertEqual(second, (target,))
            self.assertEqual(target.read_bytes(), payload)
            self.assertEqual(opener.calls, 1)
            self.assertEqual(list((root / "cache").glob(".model.onnx.*")), [])

    def test_rejects_hash_or_size_mismatch_without_installing_partial_file(self):
        expected = b"expected"
        cases = (b"tampered", b"expected plus trailing bytes")
        for payload in cases:
            with self.subTest(payload=payload), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                manifest_path = _write_manifest(root, expected)
                with self.assertRaises(DownloadError):
                    download_models(
                        manifest_path,
                        root / "cache",
                        opener=RecordingOpener(payload, chunk_size=2),
                    )
                self.assertFalse((root / "cache" / "model.onnx").exists())
                self.assertEqual(list((root / "cache").glob(".model.onnx.*")), [])

    def test_rejects_existing_mismatch_instead_of_overwriting_it(self):
        payload = b"expected"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest_path = _write_manifest(root, payload)
            cache = root / "cache"
            cache.mkdir()
            target = cache / "model.onnx"
            target.write_bytes(b"user-owned mismatch")

            with self.assertRaises(DownloadError):
                download_models(manifest_path, cache, opener=RecordingOpener(payload))

            self.assertEqual(target.read_bytes(), b"user-owned mismatch")

    def test_never_clobbers_target_that_appears_during_atomic_publish(self):
        payload = b"expected"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest_path = _write_manifest(root, payload)
            cache = root / "cache"
            target = cache / "model.onnx"
            real_link = os.link

            def contend(source, destination, *, follow_symlinks):
                Path(destination).write_bytes(b"concurrent owner")
                return real_link(
                    source, destination, follow_symlinks=follow_symlinks
                )

            with mock.patch(
                "workers.models.download.os.link", side_effect=contend
            ), self.assertRaises(DownloadError):
                download_models(
                    manifest_path,
                    cache,
                    opener=RecordingOpener(payload),
                )

            self.assertEqual(target.read_bytes(), b"concurrent owner")

    @unittest.skipUnless(hasattr(os, "symlink"), "symlink support is required")
    def test_rejects_symlink_cache_and_target(self):
        payload = b"expected"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest_path = _write_manifest(root, payload)
            real_cache = root / "real-cache"
            real_cache.mkdir()
            linked_cache = root / "linked-cache"
            linked_cache.symlink_to(real_cache, target_is_directory=True)
            with self.assertRaises(DownloadError):
                download_models(manifest_path, linked_cache, opener=RecordingOpener(payload))

            outside = root / "outside"
            outside.write_bytes(b"outside")
            target = real_cache / "model.onnx"
            target.symlink_to(outside)
            with self.assertRaises(DownloadError):
                download_models(manifest_path, real_cache, opener=RecordingOpener(payload))
            self.assertEqual(outside.read_bytes(), b"outside")

    def test_rejects_relative_cache_and_unapproved_redirects(self):
        payload = b"expected"
        with tempfile.TemporaryDirectory() as directory:
            manifest_path = _write_manifest(Path(directory), payload)
            with self.assertRaises(DownloadError):
                download_models(
                    manifest_path,
                    Path("relative-cache"),
                    opener=RecordingOpener(payload),
                )

        handler = _HTTPSRedirectHandler()
        original = Request("https://huggingface.co/opencv/model/resolve/commit/model.onnx")
        for redirected in (
            "http://cdn-lfs.hf.co/model.onnx",
            "https://example.com/model.onnx",
            "https://user@cdn-lfs.hf.co/model.onnx",
            "https://cdn-lfs.hf.co:444/model.onnx",
        ):
            with self.subTest(redirected=redirected), self.assertRaises(DownloadError):
                handler.redirect_request(original, None, 302, "Found", {}, redirected)

        modelscope = Request(
            "https://www.modelscope.cn/models/iic/model/resolve/v1.0.5/model.ckpt"
        )
        accepted = handler.redirect_request(
            modelscope,
            None,
            302,
            "Found",
            {},
            "https://cdn-lfs-cn-1.modelscope.cn/path/model.ckpt?auth_key=temporary",
        )
        self.assertEqual(accepted.full_url.partition("?")[0], "https://cdn-lfs-cn-1.modelscope.cn/path/model.ckpt")


class FakeResponse:
    def __init__(self, payload, chunk_size):
        self._stream = io.BytesIO(payload)
        self._chunk_size = chunk_size

    def read(self, size=-1):
        return self._stream.read(min(size, self._chunk_size))

    def __enter__(self):
        return self

    def __exit__(self, _type, _value, _traceback):
        return False


class RecordingOpener:
    def __init__(self, payload, chunk_size=8192):
        self._payload = payload
        self._chunk_size = chunk_size
        self.calls = 0

    def __call__(self, request, timeout):
        self.calls += 1
        self.last_request = request
        self.last_timeout = timeout
        return FakeResponse(self._payload, self._chunk_size)


def _manifest_entry(payload):
    return {
        "id": "test-model",
        "filename": "model.onnx",
        "url": "https://huggingface.co/opencv/test/resolve/commit/model.onnx",
        "size_bytes": len(payload),
        "sha256": hashlib.sha256(payload).hexdigest(),
        "upstream_sha384": None,
        "license": "MIT",
        "license_url": "https://huggingface.co/opencv/test/blob/commit/LICENSE",
    }


def _write_manifest(root, payload):
    path = root / "manifest.json"
    path.write_text(
        json.dumps({"schema_version": 1, "models": [_manifest_entry(payload)]}),
        encoding="utf-8",
    )
    return path
