# Local identity models

`manifest.v1.json` is the only model allowlist. It pins immutable download URLs,
byte sizes, digests, and licenses. Model binaries are intentionally absent from
Git and must be installed into an explicit cache:

```sh
PIE_MODEL_CACHE="$PWD/.model-cache"
.conda-gpu/bin/python -m workers.models.download --cache-dir "$PIE_MODEL_CACHE"
```

The downloader accepts only the strict schema, approved HTTPS sources and
redirects, and exact bytes. It streams into a same-directory temporary file and
publishes it with a no-clobber hard link only after all configured size and
digest checks pass. Existing
mismatched files, symlinks, relative cache paths, and path traversal are errors.

Visual inference is CUDA-only. The wrapper preloads CUDA/cuDNN from the unified
environment's NVIDIA packages, disables CPU EP fallback, and exposes an opt-in
profile audit that rejects any executed model node not assigned to
`CUDAExecutionProvider`.

Run artifact and GPU conformance with:

```sh
PIE_REAL_MODEL_DIR="$PWD/.model-cache" \
  .conda-gpu/bin/python -m unittest workers.vision.test_real_models
```

The same manifest installs the pinned ERes2Net checkpoint for the speaker
worker. Its official conformance WAV files are not runtime dependencies and
remain outside the model cache. Run the opt-in offline check with their exact
paths and an explicit CUDA device:

```sh
.conda-gpu/bin/python -m workers.speaker.conformance \
  --checkpoint .model-cache/pretrained_eres2net_aug.ckpt \
  --same-a /path/to/speaker1_a_cn_16k.wav \
  --same-b /path/to/speaker1_b_cn_16k.wav \
  --different /path/to/speaker2_a_cn_16k.wav \
  --device cuda:0 --iterations 5
```

The example ordering proves only that the pinned artifact and preprocessing
work together. It does not establish a production identification or
verification threshold.

An additional relationship check accepts a private directory through
`PIE_REAL_FACE_FIXTURE_DIR`. It expects `same-person-1.jpg`,
`same-person-2.jpg`, and `different-person.jpg`; the images are never checked
in. This check only verifies that the first two embeddings are more similar
than the different-person embedding. It does not define a product match
threshold.
