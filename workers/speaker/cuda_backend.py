"""Pinned ERes2Net CUDA backend; runtime loading is local and offline only."""

import hashlib
import pathlib
import re
import threading


MODEL_ID = "iic/speech_eres2net_sv_zh-cn_16k-common"
MODEL_REVISION = "v1.0.5"
WEIGHT_FILENAME = "pretrained_eres2net_aug.ckpt"
EXPECTED_WEIGHT_BYTES = 221_210_095
EXPECTED_WEIGHT_SHA256 = "ad78a02cab9dc23385c3fe235d1c3a370c0bd89ef192d0250546b370b0b8f227"

_CUDA_DEVICE = re.compile(r"cuda:(0|[1-9][0-9]*)\Z")


class ModelUnavailableError(RuntimeError):
    """The pinned model cannot safely provide CUDA inference."""


class CudaERes2NetBackend:
    """Extract 192-dimensional embeddings on one explicit CUDA device."""

    model_sha256 = EXPECTED_WEIGHT_SHA256

    def __init__(self, checkpoint_path, device):
        index = parse_cuda_device(device)
        path = pathlib.Path(checkpoint_path)
        try:
            import torch
            import torchaudio.compliance.kaldi as kaldi
        except Exception as exc:
            raise ModelUnavailableError("speaker CUDA runtime is unavailable") from exc
        require_cuda_device(device, torch.cuda)
        verify_checkpoint(path, EXPECTED_WEIGHT_BYTES, EXPECTED_WEIGHT_SHA256)
        try:
            state = load_checkpoint_state(torch, path)
            if not isinstance(state, dict) or not state:
                raise ModelUnavailableError("speaker checkpoint state is invalid")
            from workers.speaker._vendor.eres2net import ERes2Net

            model = ERes2Net(feat_dim=80, embedding_size=192)
            model.load_state_dict(state, strict=True)
            self._device = torch.device(f"cuda:{index}")
            model.to(self._device)
            model.eval()
            if next(model.parameters()).device.type != "cuda":
                raise ModelUnavailableError("speaker model did not activate CUDA")
        except ModelUnavailableError:
            raise
        except Exception as exc:
            raise ModelUnavailableError("speaker model could not initialize on CUDA") from exc
        self._torch = torch
        self._kaldi = kaldi
        self._model = model
        self._inference_lock = threading.Lock()

    @property
    def device(self):
        return str(self._device)

    def embed(self, pcm_s16le, sample_rate):
        if sample_rate != 16_000 or not isinstance(pcm_s16le, bytes) or len(pcm_s16le) < 800 or len(pcm_s16le) % 2 != 0:
            raise ValueError("speaker backend requires at least 25 ms of 16 kHz s16le PCM")
        try:
            import numpy

            samples = numpy.frombuffer(pcm_s16le, dtype="<i2").astype("float32", copy=True)
            waveform = self._torch.from_numpy(samples).unsqueeze(0).div_(32768.0)
            try:
                features = self._kaldi.fbank(
                    waveform,
                    num_mel_bins=80,
                    sample_frequency=16_000,
                    dither=0.0,
                )
            finally:
                waveform.zero_()
                samples.fill(0.0)
            features = features - features.mean(0, keepdim=True)
            with self._inference_lock, self._torch.inference_mode():
                embedding = self._model(features.unsqueeze(0).to(self._device))
                if embedding.device.type != "cuda":
                    raise ModelUnavailableError("speaker inference did not execute on CUDA")
                result = embedding.detach().squeeze(0).to("cpu").tolist()
            return result
        except Exception as exc:
            raise ModelUnavailableError("speaker CUDA inference failed") from exc


def parse_cuda_device(device):
    if not isinstance(device, str):
        raise ValueError("speaker device must be explicit cuda:<index>")
    match = _CUDA_DEVICE.fullmatch(device)
    if match is None:
        raise ValueError("speaker device must be explicit cuda:<index>")
    return int(match.group(1))


def require_cuda_device(device, cuda_runtime):
    index = parse_cuda_device(device)
    if not cuda_runtime.is_available() or index >= cuda_runtime.device_count():
        raise ModelUnavailableError("requested speaker CUDA device is unavailable")
    return index


def verify_checkpoint(path, expected_size=EXPECTED_WEIGHT_BYTES, expected_sha256=EXPECTED_WEIGHT_SHA256):
    path = pathlib.Path(path)
    if path.is_symlink() or not path.is_file():
        raise ModelUnavailableError("speaker checkpoint is not a regular file")
    try:
        if path.stat().st_size != expected_size:
            raise ModelUnavailableError("speaker checkpoint size mismatch")
        digest = hashlib.sha256()
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        if digest.hexdigest() != expected_sha256:
            raise ModelUnavailableError("speaker checkpoint digest mismatch")
    except OSError as exc:
        raise ModelUnavailableError("speaker checkpoint is unavailable") from exc


def load_checkpoint_state(torch_module, path):
    """Deserialize weights on host memory; model execution remains CUDA-only."""
    try:
        return torch_module.load(path, map_location="cpu", weights_only=True)
    except Exception as exc:
        raise ModelUnavailableError("speaker checkpoint failed restricted loading") from exc
