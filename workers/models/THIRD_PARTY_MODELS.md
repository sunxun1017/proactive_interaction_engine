# Third-party identity models

Model binaries are downloaded on demand into an ignored local cache. They are
not committed to this repository. `manifest.v1.json` pins the exact source,
byte size, digest, and license for every artifact.

## YuNet face detector

- Artifact: `face_detection_yunet_2023mar.onnx`
- Source: OpenCV Zoo / OpenCV Hugging Face mirror
- License: MIT
- Copyright: Shiqi Yu and contributors
- License text: <https://github.com/opencv/opencv_zoo/blob/4.10.0/models/face_detection_yunet/LICENSE>

## SFace face recognizer

- Artifact: `face_recognition_sface_2021dec.onnx`
- Source: OpenCV Zoo / OpenCV Hugging Face mirror
- License: Apache License 2.0
- License text: <https://github.com/opencv/opencv_zoo/blob/4.10.0/models/face_recognition_sface/LICENSE>

## anti-spoof-mn3

- Artifact: `anti-spoof-mn3.onnx`
- Source: OpenVINO Open Model Zoo, derived from Lightweight Face Anti Spoofing
- Original model license: MIT
- Original copyright: Prokofev Kirill
- License text: <https://raw.githubusercontent.com/kprokofi/light-weight-face-anti-spoofing/master/LICENSE>
- Open Model Zoo metadata and downloader code: Apache License 2.0

## ERes2Net speaker verification

- Artifact: `pretrained_eres2net_aug.ckpt`
- Source: ModelScope IIC `speech_eres2net_sv_zh-cn_16k-common`, revision `v1.0.5`
- Weight license: Apache License 2.0
- Official license evidence: <https://www.modelscope.cn/api/v1/models/iic/speech_eres2net_sv_zh-cn_16k-common>
- Runtime architecture source: ModelScope 3D-Speaker, Apache License 2.0
- Code license: <https://github.com/modelscope/3D-Speaker/blob/master/LICENSE>

The pinned ModelScope revision does not contain a separate `LICENSE` file, so
the official model metadata is the weight-license evidence. The 3D-Speaker
code license covers the adapted runtime implementation but is not used as a
substitute for the model-weight declaration.

These models provide evidence for local personalization. They are not safety or
security authentication mechanisms. Repository code deliberately supplies no
production detection, identification, or liveness threshold; those values
require an explicit, versioned calibration policy for the deployment camera and
population.
