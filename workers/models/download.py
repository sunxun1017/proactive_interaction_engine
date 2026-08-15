"""Strict, reproducible downloader for the checked-in identity model manifest."""

from dataclasses import dataclass
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import tempfile
from urllib.error import URLError
from urllib.parse import urlparse
from urllib.request import HTTPRedirectHandler, Request, build_opener


_ROOT_FIELDS = frozenset(("schema_version", "models"))
_MODEL_FIELDS = frozenset(
    (
        "id",
        "filename",
        "url",
        "size_bytes",
        "sha256",
        "upstream_sha384",
        "license",
        "license_url",
    )
)
_INITIAL_DOWNLOAD_HOSTS = frozenset(
    ("huggingface.co", "storage.openvinotoolkit.org", "www.modelscope.cn")
)
_EXACT_REDIRECT_HOSTS = frozenset(
    ("cas-bridge.xethub.hf.co", "cdn-lfs-cn-1.modelscope.cn")
)
_MAX_MODEL_BYTES = 512 * 1024 * 1024
_READ_CHUNK_BYTES = 1024 * 1024
_DOWNLOAD_TIMEOUT_SECONDS = 30
_MODEL_ID = re.compile(r"[a-z0-9][a-z0-9.-]{0,127}\Z")
_FILENAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,254}\Z")
_LOWER_HEX_256 = re.compile(r"[0-9a-f]{64}\Z")
_LOWER_HEX_384 = re.compile(r"[0-9a-f]{96}\Z")


class DownloadError(RuntimeError):
    """A model artifact could not be installed without weakening integrity."""


@dataclass(frozen=True)
class ModelArtifact:
    model_id: str
    filename: str
    url: str
    size_bytes: int
    sha256: str
    upstream_sha384: str | None
    license: str
    license_url: str


@dataclass(frozen=True)
class ModelManifest:
    schema_version: int
    models: tuple[ModelArtifact, ...]


def load_manifest(path):
    """Load and fully validate one schema-v1 JSON model manifest."""
    manifest_path = Path(path)
    try:
        with manifest_path.open("r", encoding="utf-8") as file:
            raw = json.load(
                file,
                object_pairs_hook=_unique_object,
                parse_constant=lambda value: _reject_json_constant(value),
            )
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError(f"read model manifest: {error}") from error
    if not isinstance(raw, dict) or frozenset(raw) != _ROOT_FIELDS:
        raise ValueError("model manifest must contain exactly schema_version and models")
    if type(raw["schema_version"]) is not int or raw["schema_version"] != 1:
        raise ValueError("model manifest schema_version must equal 1")
    if not isinstance(raw["models"], list) or not raw["models"] or len(raw["models"]) > 32:
        raise ValueError("model manifest must contain between 1 and 32 models")

    models = []
    seen_ids = set()
    seen_filenames = set()
    for raw_model in raw["models"]:
        model = _parse_model(raw_model)
        if model.model_id in seen_ids:
            raise ValueError(f"model id {model.model_id!r} is duplicated")
        if model.filename in seen_filenames:
            raise ValueError(f"model filename {model.filename!r} is duplicated")
        seen_ids.add(model.model_id)
        seen_filenames.add(model.filename)
        models.append(model)
    return ModelManifest(schema_version=1, models=tuple(models))


def download_models(manifest_path, cache_dir, model_ids=None, *, opener=None):
    """Install selected manifest artifacts atomically into an explicit cache."""
    manifest = load_manifest(manifest_path)
    selected = _select_models(manifest, model_ids)
    root = _prepare_cache_root(cache_dir)
    open_request = opener or _open_https
    installed = []
    for model in selected:
        target = root / model.filename
        _ensure_direct_child(root, target)
        if _path_exists(target):
            _reject_symlink_or_nonregular(target, "model target")
            if not _matches(target, model):
                raise DownloadError(f"existing model {model.model_id!r} does not match manifest")
            installed.append(target)
            continue
        installed.append(_download_one(model, root, target, open_request))
    return tuple(installed)


def _parse_model(raw):
    if not isinstance(raw, dict) or frozenset(raw) != _MODEL_FIELDS:
        raise ValueError("model entry contains missing or unknown fields")
    model_id = raw["id"]
    filename = raw["filename"]
    size_bytes = raw["size_bytes"]
    sha256 = raw["sha256"]
    sha384 = raw["upstream_sha384"]
    license_name = raw["license"]
    if not isinstance(model_id, str) or _MODEL_ID.fullmatch(model_id) is None:
        raise ValueError("model id is invalid")
    if (
        not isinstance(filename, str)
        or _FILENAME.fullmatch(filename) is None
        or Path(filename).name != filename
    ):
        raise ValueError("model filename must be one safe path component")
    if type(size_bytes) is not int or size_bytes <= 0 or size_bytes > _MAX_MODEL_BYTES:
        raise ValueError("model size is outside the supported range")
    if not isinstance(sha256, str) or _LOWER_HEX_256.fullmatch(sha256) is None:
        raise ValueError("model sha256 is invalid")
    if sha384 is not None and (
        not isinstance(sha384, str) or _LOWER_HEX_384.fullmatch(sha384) is None
    ):
        raise ValueError("model upstream_sha384 is invalid")
    if (
        not isinstance(license_name, str)
        or not license_name
        or license_name.strip() != license_name
    ):
        raise ValueError("model license is invalid")
    _validate_url(raw["url"], download=True)
    _validate_url(raw["license_url"], download=False)
    return ModelArtifact(
        model_id=model_id,
        filename=filename,
        url=raw["url"],
        size_bytes=size_bytes,
        sha256=sha256,
        upstream_sha384=sha384,
        license=license_name,
        license_url=raw["license_url"],
    )


def _validate_url(value, *, download):
    if not isinstance(value, str) or not value or value.strip() != value:
        raise ValueError("model URL is invalid")
    parsed = urlparse(value)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.port is not None
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError("model URLs must be plain HTTPS URLs")
    if download and parsed.hostname not in _INITIAL_DOWNLOAD_HOSTS:
        raise ValueError(f"model download host {parsed.hostname!r} is not approved")


def _select_models(manifest, model_ids):
    if model_ids is None:
        return manifest.models
    requested = tuple(model_ids)
    if not requested or any(not isinstance(item, str) for item in requested):
        raise ValueError("at least one model id is required when selecting models")
    if len(set(requested)) != len(requested):
        raise ValueError("selected model ids must be unique")
    by_id = {model.model_id: model for model in manifest.models}
    unknown = [model_id for model_id in requested if model_id not in by_id]
    if unknown:
        raise ValueError(f"unknown model id {unknown[0]!r}")
    return tuple(by_id[model_id] for model_id in requested)


def _prepare_cache_root(cache_dir):
    root = Path(cache_dir)
    if not root.is_absolute():
        raise DownloadError("model cache directory must be absolute")
    _reject_existing_symlink_components(root)
    try:
        root.mkdir(mode=0o755, parents=True, exist_ok=True)
    except OSError as error:
        raise DownloadError(f"create model cache: {error}") from error
    _reject_existing_symlink_components(root)
    try:
        mode = root.stat().st_mode
    except OSError as error:
        raise DownloadError(f"inspect model cache: {error}") from error
    if not stat.S_ISDIR(mode):
        raise DownloadError("model cache path is not a directory")
    return root


def _download_one(model, root, target, opener):
    request = Request(model.url, headers={"User-Agent": "proactive-engine-model-fetch/1"})
    temporary_path = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w+b", prefix=f".{model.filename}.", dir=root, delete=False
        ) as temporary:
            temporary_path = Path(temporary.name)
            sha256 = hashlib.sha256()
            sha384 = hashlib.sha384() if model.upstream_sha384 is not None else None
            total = 0
            try:
                response_context = opener(request, timeout=_DOWNLOAD_TIMEOUT_SECONDS)
                with response_context as response:
                    while True:
                        chunk = response.read(_READ_CHUNK_BYTES)
                        if not chunk:
                            break
                        if not isinstance(chunk, bytes):
                            raise DownloadError("model download returned non-byte content")
                        total += len(chunk)
                        if total > model.size_bytes:
                            raise DownloadError(f"model {model.model_id!r} exceeds manifest size")
                        sha256.update(chunk)
                        if sha384 is not None:
                            sha384.update(chunk)
                        temporary.write(chunk)
            except (OSError, URLError) as error:
                raise DownloadError(f"download model {model.model_id!r}: {error}") from error
            if total != model.size_bytes or sha256.hexdigest() != model.sha256:
                raise DownloadError(f"model {model.model_id!r} failed size or sha256 verification")
            if sha384 is not None and sha384.hexdigest() != model.upstream_sha384:
                raise DownloadError(f"model {model.model_id!r} failed upstream sha384 verification")
            temporary.flush()
            os.fsync(temporary.fileno())
            os.fchmod(temporary.fileno(), 0o644)
        try:
            os.link(temporary_path, target, follow_symlinks=False)
        except FileExistsError:
            _reject_symlink_or_nonregular(target, "model target")
            if _matches(target, model):
                temporary_path.unlink()
                temporary_path = None
                return target
            raise DownloadError(f"model target {target.name!r} appeared with different content")
        temporary_path.unlink()
        temporary_path = None
        _fsync_directory(root)
        return target
    except DownloadError:
        raise
    except OSError as error:
        raise DownloadError(f"install model {model.model_id!r}: {error}") from error
    finally:
        if temporary_path is not None:
            try:
                temporary_path.unlink()
            except FileNotFoundError:
                pass


def _matches(path, model):
    try:
        if path.stat().st_size != model.size_bytes:
            return False
        sha256 = hashlib.sha256()
        sha384 = hashlib.sha384() if model.upstream_sha384 is not None else None
        with path.open("rb") as file:
            while True:
                chunk = file.read(_READ_CHUNK_BYTES)
                if not chunk:
                    break
                sha256.update(chunk)
                if sha384 is not None:
                    sha384.update(chunk)
    except OSError as error:
        raise DownloadError(f"verify existing model {model.model_id!r}: {error}") from error
    if sha256.hexdigest() != model.sha256:
        return False
    return sha384 is None or sha384.hexdigest() == model.upstream_sha384


def _path_exists(path):
    try:
        path.lstat()
        return True
    except FileNotFoundError:
        return False
    except OSError as error:
        raise DownloadError(f"inspect model target: {error}") from error


def _reject_symlink_or_nonregular(path, label):
    try:
        mode = path.lstat().st_mode
    except OSError as error:
        raise DownloadError(f"inspect {label}: {error}") from error
    if stat.S_ISLNK(mode):
        raise DownloadError(f"{label} must not be a symlink")
    if not stat.S_ISREG(mode):
        raise DownloadError(f"{label} must be a regular file")


def _reject_existing_symlink_components(path):
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            mode = current.lstat().st_mode
        except FileNotFoundError:
            continue
        except OSError as error:
            raise DownloadError(f"inspect model cache path: {error}") from error
        if stat.S_ISLNK(mode):
            raise DownloadError("model cache path must not contain symlinks")


def _ensure_direct_child(root, target):
    if target.parent != root or target.name in ("", ".", ".."):
        raise DownloadError("model target escapes cache directory")


def _fsync_directory(directory):
    descriptor = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError(f"JSON field {key!r} is duplicated")
        value[key] = item
    return value


def _reject_json_constant(value):
    raise ValueError(f"JSON constant {value!r} is not permitted")


def _redirect_host_allowed(hostname):
    return (
        hostname in _INITIAL_DOWNLOAD_HOSTS
        or hostname in _EXACT_REDIRECT_HOSTS
        or hostname == "hf.co"
        or hostname.endswith(".hf.co")
        or hostname.endswith(".huggingface.co")
    )


class _HTTPSRedirectHandler(HTTPRedirectHandler):
    def redirect_request(self, request, file_pointer, code, message, headers, new_url):
        parsed = urlparse(new_url)
        if (
            parsed.scheme != "https"
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
            or parsed.port is not None
            or parsed.fragment
            or not _redirect_host_allowed(parsed.hostname)
        ):
            raise DownloadError("model download redirected outside approved HTTPS hosts")
        return super().redirect_request(request, file_pointer, code, message, headers, new_url)


def _open_https(request, timeout):
    return build_opener(_HTTPSRedirectHandler()).open(request, timeout=timeout)


def main(argv=None):
    parser = argparse.ArgumentParser(description="download verified local identity models")
    parser.add_argument("--cache-dir", type=Path, required=True)
    parser.add_argument(
        "--manifest", type=Path, default=Path(__file__).with_name("manifest.v1.json")
    )
    parser.add_argument("model_id", nargs="*")
    args = parser.parse_args(argv)
    selected = args.model_id or None
    for installed in download_models(args.manifest, args.cache_dir.resolve(), selected):
        print(installed)


if __name__ == "__main__":
    main()
