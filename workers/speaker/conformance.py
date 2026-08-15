"""Opt-in offline CUDA conformance for the pinned official speaker model."""

import argparse
import hashlib
import json
import math
import pathlib
import resource
import shutil
import subprocess
import time
import wave

from workers.speaker.cuda_backend import CudaERes2NetBackend, ModelUnavailableError, parse_cuda_device
from workers.speaker.model import SpeakerModel, affine_cosine_score


_MAX_WAV_FRAMES = 90 * 16_000
_OFFICIAL_EXAMPLES = {
    "same-a": (
        118_932,
        "5f20ce0ddc378ca3239d3ce864b1142726a46a1221ae553912e4e142045df58b",
    ),
    "same-b": (
        157_058,
        "20745dc08a4281894d146140b99b9ef7417ac681119b7f7202f553cdf1a85f65",
    ),
    "different": (
        170_028,
        "8a6cffa452df32ef10503f7992f22ffcdd7f16c4e0273d13311bc5cdcb13abf4",
    ),
}


def read_s16le_mono_16k_wav(path):
    path = pathlib.Path(path)
    try:
        with wave.open(str(path), "rb") as source:
            compatible = (
                source.getnchannels() == 1
                and source.getsampwidth() == 2
                and source.getframerate() == 16_000
                and source.getcomptype() == "NONE"
            )
            frame_count = source.getnframes()
            if not compatible or not 1 <= frame_count <= _MAX_WAV_FRAMES:
                raise ValueError("speaker conformance WAV must be bounded mono 16 kHz s16le PCM")
            payload = source.readframes(frame_count)
    except (OSError, EOFError, wave.Error) as exc:
        raise ValueError("speaker conformance WAV is unreadable") from exc
    if len(payload) != frame_count * 2:
        raise ValueError("speaker conformance WAV is truncated")
    return payload


def latency_summary(milliseconds):
    values = sorted(float(value) for value in milliseconds)
    if not values or not all(math.isfinite(value) and value >= 0.0 for value in values):
        raise ValueError("speaker latency samples are invalid")

    def nearest_rank(quantile):
        return values[max(0, math.ceil(quantile * len(values)) - 1)]

    return {
        "count": len(values),
        "min_ms": values[0],
        "p50_ms": nearest_rank(0.50),
        "p95_ms": nearest_rank(0.95),
        "p99_ms": nearest_rank(0.99),
        "max_ms": values[-1],
    }


def run_conformance(checkpoint_path, same_a, same_b, different, device, iterations=5):
    """Run local example ordering and bounded performance measurements without thresholds."""
    if type(iterations) is not int or not 1 <= iterations <= 100:
        raise ValueError("speaker conformance iterations must be within [1, 100]")
    device_index = parse_cuda_device(device)
    paths = {"same-a": same_a, "same-b": same_b, "different": different}
    for role, path in paths.items():
        expected_size, expected_sha256 = _OFFICIAL_EXAMPLES[role]
        verify_conformance_asset(path, expected_size, expected_sha256)
    audio_a = read_s16le_mono_16k_wav(same_a)
    audio_b = read_s16le_mono_16k_wav(same_b)
    audio_different = read_s16le_mono_16k_wav(different)

    rss_before_kib = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    load_started = time.perf_counter()
    backend = CudaERes2NetBackend(checkpoint_path, device)
    model = SpeakerModel(backend)
    load_ms = (time.perf_counter() - load_started) * 1_000.0

    import torch

    torch.cuda.reset_peak_memory_stats(device_index)
    embeddings = []
    latencies = []
    for _ in range(iterations):
        torch.cuda.synchronize(device_index)
        started = time.perf_counter()
        embeddings.append(model.extract(audio_a))
        torch.cuda.synchronize(device_index)
        latencies.append((time.perf_counter() - started) * 1_000.0)

    embedding_b = model.extract(audio_b)
    embedding_different = model.extract(audio_different)
    reference = embeddings[0]
    determinism_delta = max(
        abs(reference[index] - embedding[index])
        for embedding in embeddings[1:]
        for index in range(len(reference))
    ) if len(embeddings) > 1 else 0.0
    same_score = affine_cosine_score(sum(left * right for left, right in zip(reference, embedding_b)))
    different_score = affine_cosine_score(
        sum(left * right for left, right in zip(reference, embedding_different))
    )

    properties = torch.cuda.get_device_properties(device_index)
    rss_after_kib = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    return {
        "device": {
            "requested": device,
            "name": properties.name,
            "compute_capability": f"{properties.major}.{properties.minor}",
            "total_memory_bytes": properties.total_memory,
            "driver_version": _nvidia_driver_version(device_index),
            "torch": torch.__version__,
            "torch_cuda": torch.version.cuda,
            "cudnn": torch.backends.cudnn.version(),
        },
        "model": {
            "sha256": model.model_sha256,
            "load_ms": load_ms,
        },
        "latency": latency_summary(latencies),
        "rss_peak_kib": rss_after_kib,
        "rss_peak_delta_kib": max(0, rss_after_kib - rss_before_kib),
        "cuda_peak_allocated_bytes": torch.cuda.max_memory_allocated(device_index),
        "determinism_max_abs_delta": determinism_delta,
        "scores": {
            "same_speaker": same_score,
            "different_speaker": different_score,
        },
    }


def verify_conformance_asset(path, expected_size, expected_sha256):
    path = pathlib.Path(path)
    if path.is_symlink() or not path.is_file():
        raise ValueError("speaker conformance asset must be a regular file")
    if type(expected_size) is not int or expected_size <= 0:
        raise ValueError("speaker conformance asset size is invalid")
    if (
        not isinstance(expected_sha256, str)
        or len(expected_sha256) != 64
        or expected_sha256 != expected_sha256.lower()
    ):
        raise ValueError("speaker conformance asset digest is invalid")
    try:
        digest = hashlib.sha256()
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        if path.stat().st_size != expected_size or digest.hexdigest() != expected_sha256:
            raise ValueError("speaker conformance asset does not match the official pin")
    except OSError as exc:
        raise ValueError("speaker conformance asset is unreadable") from exc


def _nvidia_driver_version(device_index):
    binary = shutil.which("nvidia-smi")
    if binary is None:
        raise ModelUnavailableError("nvidia-smi is required for conformance metadata")
    try:
        result = subprocess.run(
            [
                binary,
                f"--id={device_index}",
                "--query-gpu=driver_version",
                "--format=csv,noheader",
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise ModelUnavailableError("could not read NVIDIA driver metadata") from exc
    version = result.stdout.strip()
    if not version or "\n" in version:
        raise ModelUnavailableError("NVIDIA driver metadata is invalid")
    return version


def main(argv=None):
    parser = argparse.ArgumentParser(description="offline CUDA conformance for pinned ERes2Net speaker model")
    parser.add_argument("--checkpoint", required=True, type=pathlib.Path)
    parser.add_argument("--same-a", required=True, type=pathlib.Path)
    parser.add_argument("--same-b", required=True, type=pathlib.Path)
    parser.add_argument("--different", required=True, type=pathlib.Path)
    parser.add_argument("--device", required=True, help="explicit CUDA device such as cuda:0")
    parser.add_argument("--iterations", type=int, default=5)
    args = parser.parse_args(argv)
    report = run_conformance(
        checkpoint_path=args.checkpoint,
        same_a=args.same_a,
        same_b=args.same_b,
        different=args.different,
        device=args.device,
        iterations=args.iterations,
    )
    print(json.dumps(report, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
