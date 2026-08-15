package identitycalibration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"

	"proactive-interaction-engine/internal/domain/fault"
)

const (
	loadOp                      = "load identity calibration artifact"
	supportedSchemaVersion      = uint32(1)
	maximumArtifactBytes        = 1 << 20
	maximumMachineIDBytes       = 128
	maximumExactStringBytes     = 128
	maximumScorePPM             = uint32(1_000_000)
	maximumCUDADeviceIndex      = uint32(31)
	maximumDetectionTopK        = uint32(4096)
	maximumEnrollmentClips      = uint32(16)
	maximumDurationMicroseconds = uint64(math.MaxInt64 / 1000)

	usageScopePersonalizationOnly = "PERSONALIZATION_ONLY"
	inferenceModeCUDAOnly         = "CUDA_ONLY"
	quantileRuleNearestRankV1     = "NEAREST_RANK_V1"
	measurementBoundaryV1         = "CAPTURE_OR_WORK_TO_EVIDENCE_TERMINAL_V1"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var expectedModelRoles = [...]ModelRole{
	ModelRoleFaceDetection,
	ModelRoleFaceEmbedding,
	ModelRoleFaceLiveness,
	ModelRoleSpeakerEmbedding,
}

var expectedCapabilities = [...]Capability{
	CapabilityFaceDetection,
	CapabilityFaceIdentification,
	CapabilityFaceLiveness,
	CapabilitySpeakerIdentification,
	CapabilitySpeakerVerification,
}

// Load decodes exactly one bounded, strict schema-v1 JSON artifact.
func Load(reader io.Reader) (LoadedArtifact, error) {
	if reader == nil {
		return LoadedArtifact{}, invalidInput(errors.New("reader is required"))
	}
	content, err := io.ReadAll(io.LimitReader(reader, maximumArtifactBytes+1))
	if err != nil {
		return LoadedArtifact{}, invalidInput(fmt.Errorf("read artifact: %w", err))
	}
	if len(content) > maximumArtifactBytes {
		return LoadedArtifact{}, invalidInput(fmt.Errorf("artifact exceeds %d bytes", maximumArtifactBytes))
	}
	document, err := parseStrictJSON(content)
	if err != nil {
		return LoadedArtifact{}, invalidInput(err)
	}
	if err := validateDocumentShape(document); err != nil {
		return LoadedArtifact{}, invalidInput(err)
	}

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return LoadedArtifact{}, invalidInput(fmt.Errorf("decode artifact: %w", err))
	}
	if err := validateArtifact(artifact); err != nil {
		return LoadedArtifact{}, invalidInput(err)
	}
	hash, err := canonicalHash(artifact)
	if err != nil {
		return LoadedArtifact{}, invalidInput(err)
	}
	return LoadedArtifact{SchemaVersion: artifact.SchemaVersion, Artifact: artifact, Hash: hash}, nil
}

func parseStrictJSON(content []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	value, err := parseJSONValue(decoder, "$")
	if err != nil {
		return nil, fmt.Errorf("decode strict JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode strict JSON: trailing value is not allowed")
		}
		return nil, fmt.Errorf("decode strict JSON trailing value: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("artifact root must be an object")
	}
	return value, nil
}

func parseJSONValue(decoder *json.Decoder, path string) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("%s object key must be a string", path)
				}
				if _, duplicate := object[key]; duplicate {
					return nil, fmt.Errorf("%s contains duplicate field %q", path, key)
				}
				value, err := parseJSONValue(decoder, path+"."+key)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return nil, errors.New("object is not closed")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				value, err := parseJSONValue(decoder, fmt.Sprintf("%s[%d]", path, len(array)))
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return nil, errors.New("array is not closed")
			}
			return array, nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", typed)
		}
	case json.Number:
		value := typed.String()
		if !validUnsignedDecimal(value) {
			return nil, fmt.Errorf("%s must use an unsigned decimal integer", path)
		}
		return typed, nil
	case nil:
		return nil, fmt.Errorf("%s must not be null", path)
	case string, bool:
		return typed, nil
	default:
		return nil, fmt.Errorf("%s has unsupported JSON value", path)
	}
}

func validUnsignedDecimal(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for index := 1; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func validateDocumentShape(document any) error {
	root, err := requireFields(document, "$", "schema_version", "calibration_id", "policy_version", "usage_scope", "security_authentication_allowed", "models", "runtime", "protocols", "application_policy", "provider_latency", "provenance")
	if err != nil {
		return err
	}
	models, ok := root["models"].([]any)
	if !ok {
		return errors.New("$.models must be an array")
	}
	for index, model := range models {
		if _, err := requireFields(model, fmt.Sprintf("$.models[%d]", index), "role", "model_id", "sha256"); err != nil {
			return err
		}
	}
	if _, err := requireFields(root["runtime"], "$.runtime", "inference_mode", "cuda_required", "cpu_fallback_allowed", "cuda_device_index", "target_hardware_profile_id", "os_release", "architecture", "gpu_vendor", "gpu_model", "gpu_compute_capability", "gpu_memory_bytes", "driver_version", "cuda_version", "cudnn_version", "python_version", "onnxruntime_gpu_version", "opencv_version", "torch_version", "torchaudio_version", "camera_profile_id", "microphone_profile_id"); err != nil {
		return err
	}
	protocols, err := requireFields(root["protocols"], "$.protocols", "face_detection", "face_identification", "face_liveness", "speaker")
	if err != nil {
		return err
	}
	if _, err := requireFields(protocols["face_detection"], "$.protocols.face_detection", "protocol_id", "input_width", "input_height", "pixel_format", "resize_rule", "tensor_rule", "score_mapping", "score_threshold_ppm", "nms_rule", "nms_threshold_ppm", "top_k"); err != nil {
		return err
	}
	if _, err := requireFields(protocols["face_identification"], "$.protocols.face_identification", "protocol_id", "alignment_rule", "pixel_rule", "embedding_rule", "score_mapping"); err != nil {
		return err
	}
	if _, err := requireFields(protocols["face_liveness"], "$.protocols.face_liveness", "protocol_id", "crop_rule", "input_rule", "normalization_rule", "score_mapping", "real_output_index", "spoof_output_index", "decision_rule", "threshold_ppm"); err != nil {
		return err
	}
	if _, err := requireFields(protocols["speaker"], "$.protocols.speaker", "protocol_id", "pcm_rule", "clip_rule_id", "minimum_clip_us", "maximum_clip_us", "minimum_voiced_us", "maximum_leading_silence_us", "maximum_trailing_silence_us", "vad_rule_id", "fbank_rule_id", "enrollment_minimum_clips", "enrollment_maximum_clips", "enrollment_aggregation_rule", "score_mapping"); err != nil {
		return err
	}
	if _, err := requireFields(root["application_policy"], "$.application_policy", "face_identification_threshold_ppm", "speaker_identification_threshold_ppm", "speaker_verification_threshold_ppm", "require_face_liveness"); err != nil {
		return err
	}
	latencies, ok := root["provider_latency"].([]any)
	if !ok {
		return errors.New("$.provider_latency must be an array")
	}
	for index, latency := range latencies {
		if _, err := requireFields(latency, fmt.Sprintf("$.provider_latency[%d]", index), "capability", "provider_id", "sample_count", "quantile_rule", "measurement_boundary", "p95_us", "p99_us", "observed_maximum_us", "declared_maximum_us"); err != nil {
			return err
		}
	}
	_, err = requireFields(root["provenance"], "$.provenance", "aggregate_curve_report_sha256", "aggregate_latency_report_sha256")
	return err
}

func requireFields(value any, path string, fields ...string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	expected := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		expected[field] = struct{}{}
		if _, exists := object[field]; !exists {
			return nil, fmt.Errorf("%s requires field %q", path, field)
		}
	}
	for field := range object {
		if _, exists := expected[field]; !exists {
			return nil, fmt.Errorf("%s contains unknown field %q", path, field)
		}
	}
	return object, nil
}

func validateArtifact(artifact Artifact) error {
	if artifact.SchemaVersion != supportedSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", artifact.SchemaVersion)
	}
	if !validMachineID(artifact.CalibrationID) || !validMachineID(artifact.PolicyVersion) {
		return errors.New("calibration_id and policy_version must be non-empty machine IDs")
	}
	if artifact.UsageScope != usageScopePersonalizationOnly || artifact.SecurityAuthenticationAllowed {
		return errors.New("identity calibration is restricted to personalization and never security authentication")
	}
	if err := validateModels(artifact.Models); err != nil {
		return err
	}
	if err := validateRuntime(artifact.Runtime); err != nil {
		return err
	}
	if err := validateProtocols(artifact.Protocols); err != nil {
		return err
	}
	if err := validateApplicationPolicy(artifact.ApplicationPolicy); err != nil {
		return err
	}
	if err := validateProviderLatency(artifact.ProviderLatency); err != nil {
		return err
	}
	if !validSHA256(artifact.Provenance.AggregateCurveReportSHA256) || !validSHA256(artifact.Provenance.AggregateLatencyReportSHA256) {
		return errors.New("aggregate report provenance requires lowercase SHA-256 values")
	}
	return nil
}

func validateModels(models []ModelBinding) error {
	if len(models) != len(expectedModelRoles) {
		return fmt.Errorf("models must contain exactly %d ordered roles", len(expectedModelRoles))
	}
	ids := make(map[string]struct{}, len(models))
	digests := make(map[string]struct{}, len(models))
	for index, model := range models {
		if model.Role != expectedModelRoles[index] {
			return fmt.Errorf("models[%d] role must be %s", index, expectedModelRoles[index])
		}
		if !validMachineID(model.ModelID) || !validSHA256(model.SHA256) {
			return fmt.Errorf("models[%d] requires a valid model_id and lowercase SHA-256", index)
		}
		if _, duplicate := ids[model.ModelID]; duplicate {
			return fmt.Errorf("duplicate model_id %q", model.ModelID)
		}
		if _, duplicate := digests[model.SHA256]; duplicate {
			return fmt.Errorf("duplicate model SHA-256 for role %s", model.Role)
		}
		ids[model.ModelID] = struct{}{}
		digests[model.SHA256] = struct{}{}
	}
	return nil
}

func validateRuntime(runtime RuntimeBinding) error {
	if runtime.InferenceMode != inferenceModeCUDAOnly || !runtime.CUDARequired || runtime.CPUFallbackAllowed {
		return errors.New("runtime must require CUDA_ONLY with CPU fallback disabled")
	}
	if runtime.CUDADeviceIndex > maximumCUDADeviceIndex || runtime.GPUMemoryBytes == 0 {
		return errors.New("runtime requires a bounded CUDA device and non-zero GPU memory")
	}
	if !validMachineID(runtime.TargetHardwareProfileID) || !validMachineID(runtime.CameraProfileID) || !validMachineID(runtime.MicrophoneProfileID) {
		return errors.New("runtime hardware and capture profile IDs are invalid")
	}
	values := []string{
		runtime.OSRelease, runtime.Architecture, runtime.GPUVendor, runtime.GPUModel,
		runtime.GPUComputeCapability, runtime.DriverVersion, runtime.CUDAVersion,
		runtime.CUDNNVersion, runtime.PythonVersion, runtime.ONNXRuntimeGPUVersion,
		runtime.OpenCVVersion, runtime.TorchVersion, runtime.TorchaudioVersion,
	}
	for _, value := range values {
		if !validExactString(value) {
			return errors.New("runtime contains an invalid explicit hardware or software value")
		}
	}
	if runtime.GPUVendor != "NVIDIA" || runtime.Architecture != "amd64" || !strings.HasPrefix(runtime.OSRelease, "ubuntu-") {
		return errors.New("schema v1 target runtime must be Ubuntu amd64 on NVIDIA CUDA")
	}
	return nil
}

func validateProtocols(protocols ProtocolBindings) error {
	detection := protocols.FaceDetection
	if detection.ProtocolID != "yunet-detection.v1" || detection.InputWidth != 640 || detection.InputHeight != 640 ||
		detection.PixelFormat != "BGR_U8" || detection.ResizeRule != "ASPECT_FIT_ZERO_PAD_TOP_LEFT_V1" ||
		detection.TensorRule != "NCHW_FLOAT32_SCALE_1_MEAN_0_V1" || detection.ScoreMapping != ScoreMappingSqrtClassObjectnessV1 ||
		detection.NMSRule != "STABLE_SCORE_DESC_ORDINAL_INTEGER_BOX_IOU_LT_V1" {
		return errors.New("face detection protocol does not match supported v1 rules")
	}
	if detection.ScoreThresholdPPM > maximumScorePPM || detection.NMSThresholdPPM > maximumScorePPM || detection.TopK == 0 || detection.TopK > maximumDetectionTopK {
		return errors.New("face detection thresholds or top_k are outside supported bounds")
	}
	identification := protocols.FaceIdentification
	if identification.ProtocolID != "sface-identification.v1" || identification.AlignmentRule != "SFACE_5_POINT_SIMILARITY_112X112_V1" ||
		identification.PixelRule != "BGR_TO_RGB_NCHW_FLOAT32_SCALE_1_MEAN_0_V1" || identification.EmbeddingRule != "L2_NORMALIZED_128D_V1" ||
		identification.ScoreMapping != ScoreMappingAffineCosineV1 {
		return errors.New("face identification requires the explicit AFFINE_COSINE_V1 protocol")
	}
	liveness := protocols.FaceLiveness
	if liveness.ProtocolID != "anti-spoof-liveness.v1" || liveness.CropRule != "CLIPPED_DETECTED_FACE_BOX_V1" ||
		liveness.InputRule != "BGR_TO_RGB_128X128_FLOAT32_V1" || liveness.NormalizationRule != "RGB_MEAN_1512405_1195950_1078395_SCALE_630105_564570_550035_V1" ||
		liveness.ScoreMapping != ScoreMappingRealOutputIndex0V1 || liveness.RealOutputIndex != 0 || liveness.SpoofOutputIndex != 1 ||
		liveness.DecisionRule != "REAL_SCORE_GTE_THRESHOLD_V1" || liveness.ThresholdPPM == 0 || liveness.ThresholdPPM > maximumScorePPM {
		return errors.New("face liveness protocol does not match supported v1 rules")
	}
	speaker := protocols.Speaker
	if speaker.ProtocolID != "eres2net-speaker.v1" || speaker.PCMRule != "MONO_S16LE_16000HZ_V1" ||
		speaker.ClipRuleID != "FIXED_BOUNDED_VOICED_CLIP_V1" || speaker.VADRuleID != "WEBRTC_VAD_MODE_2_20MS_V1" ||
		speaker.FBankRuleID != "TORCHAUDIO_KALDI_80_BIN_EXPLICIT_V1" || speaker.EnrollmentAggregationRule != "MEAN_THEN_L2_NORMALIZE_V1" ||
		speaker.ScoreMapping != ScoreMappingAffineCosineV1 {
		return errors.New("speaker protocol does not match supported v1 rules")
	}
	if speaker.MinimumClipUS == 0 || speaker.MinimumClipUS > speaker.MaximumClipUS || speaker.MaximumClipUS > maximumDurationMicroseconds ||
		speaker.MinimumVoicedUS == 0 || speaker.MinimumVoicedUS > speaker.MaximumClipUS || speaker.MaximumLeadingSilenceUS > speaker.MaximumClipUS ||
		speaker.MaximumTrailingSilenceUS > speaker.MaximumClipUS || speaker.EnrollmentMinimumClips == 0 ||
		speaker.EnrollmentMinimumClips > speaker.EnrollmentMaximumClips || speaker.EnrollmentMaximumClips > maximumEnrollmentClips {
		return errors.New("speaker clip or enrollment values are outside supported bounds")
	}
	return nil
}

func validateApplicationPolicy(policy ApplicationPolicy) error {
	thresholds := [...]uint32{
		policy.FaceIdentificationThresholdPPM,
		policy.SpeakerIdentificationThresholdPPM,
		policy.SpeakerVerificationThresholdPPM,
	}
	for _, threshold := range thresholds {
		if threshold == 0 || threshold > maximumScorePPM {
			return errors.New("application identity thresholds must be within (0, 1000000] ppm")
		}
	}
	if !policy.RequireFaceLiveness {
		return errors.New("application identity policy must require face liveness")
	}
	return nil
}

func validateProviderLatency(latencies []ProviderLatency) error {
	if len(latencies) != len(expectedCapabilities) {
		return fmt.Errorf("provider_latency must contain exactly %d ordered capabilities", len(expectedCapabilities))
	}
	providerIDs := make(map[string]struct{}, len(latencies))
	for index, latency := range latencies {
		if latency.Capability != expectedCapabilities[index] {
			return fmt.Errorf("provider_latency[%d] capability must be %s", index, expectedCapabilities[index])
		}
		if !validMachineID(latency.ProviderID) {
			return fmt.Errorf("provider_latency[%d] provider_id is invalid", index)
		}
		if _, duplicate := providerIDs[latency.ProviderID]; duplicate {
			return fmt.Errorf("duplicate provider_id %q", latency.ProviderID)
		}
		providerIDs[latency.ProviderID] = struct{}{}
		if latency.SampleCount == 0 || latency.QuantileRule != quantileRuleNearestRankV1 || latency.MeasurementBoundary != measurementBoundaryV1 {
			return fmt.Errorf("provider_latency[%d] measurement contract is invalid", index)
		}
		if latency.P95US == 0 || latency.P95US > latency.P99US || latency.P99US > latency.ObservedMaximumUS ||
			latency.ObservedMaximumUS > latency.DeclaredMaximumUS || latency.DeclaredMaximumUS > maximumDurationMicroseconds {
			return fmt.Errorf("provider_latency[%d] values must satisfy 0 < p95 <= p99 <= observed <= declared", index)
		}
	}
	return nil
}

func canonicalHash(artifact Artifact) (string, error) {
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("encode artifact: %w", err)
	}
	document, err := parseStrictJSON(encoded)
	if err != nil {
		return "", fmt.Errorf("canonicalize artifact: %w", err)
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode canonical artifact: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validMachineID(value string) bool {
	if len(value) == 0 || len(value) > maximumMachineIDBytes || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validExactString(value string) bool {
	if len(value) == 0 || len(value) > maximumExactStringBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune(" ._+:/()-", character) {
			continue
		}
		return false
	}
	return true
}

func validSHA256(value string) bool { return sha256Pattern.MatchString(value) }

func invalidInput(err error) error { return fault.New(fault.InvalidInput, loadOp, err) }
