package identitycalibration

// ModelRole is one required model function in a calibration artifact.
type ModelRole string

const (
	ModelRoleFaceDetection    ModelRole = "FACE_DETECTION"
	ModelRoleFaceEmbedding    ModelRole = "FACE_EMBEDDING"
	ModelRoleFaceLiveness     ModelRole = "FACE_LIVENESS"
	ModelRoleSpeakerEmbedding ModelRole = "SPEAKER_EMBEDDING"
)

// Capability identifies one independently measured biometric Provider.
type Capability string

const (
	CapabilityFaceDetection         Capability = "FACE_DETECTION"
	CapabilityFaceIdentification    Capability = "FACE_IDENTIFICATION"
	CapabilityFaceLiveness          Capability = "FACE_LIVENESS"
	CapabilitySpeakerIdentification Capability = "SPEAKER_IDENTIFICATION"
	CapabilitySpeakerVerification   Capability = "SPEAKER_VERIFICATION"
)

// ScoreMapping is an explicit, non-probabilistic score transformation.
type ScoreMapping string

const (
	ScoreMappingSqrtClassObjectnessV1 ScoreMapping = "SQRT_CLASS_OBJECTNESS_V1"
	ScoreMappingAffineCosineV1        ScoreMapping = "AFFINE_COSINE_V1"
	ScoreMappingRealOutputIndex0V1    ScoreMapping = "REAL_OUTPUT_INDEX_0_V1"
)

// LoadedArtifact is one validated schema-v1 artifact and the SHA-256 of its
// complete canonical semantic content.
type LoadedArtifact struct {
	SchemaVersion uint32
	Artifact      Artifact
	Hash          string
}

// Artifact contains every schema-v1 calibration input. No field has a default.
type Artifact struct {
	SchemaVersion                 uint32            `json:"schema_version"`
	CalibrationID                 string            `json:"calibration_id"`
	PolicyVersion                 string            `json:"policy_version"`
	UsageScope                    string            `json:"usage_scope"`
	SecurityAuthenticationAllowed bool              `json:"security_authentication_allowed"`
	Models                        []ModelBinding    `json:"models"`
	Runtime                       RuntimeBinding    `json:"runtime"`
	Protocols                     ProtocolBindings  `json:"protocols"`
	ApplicationPolicy             ApplicationPolicy `json:"application_policy"`
	ProviderLatency               []ProviderLatency `json:"provider_latency"`
	Provenance                    Provenance        `json:"provenance"`
}

// ModelBinding pins one functional role to one immutable model artifact.
type ModelBinding struct {
	Role    ModelRole `json:"role"`
	ModelID string    `json:"model_id"`
	SHA256  string    `json:"sha256"`
}

// RuntimeBinding pins CUDA execution to a reviewed deployment hardware and
// software profile without recording unique hardware identifiers.
type RuntimeBinding struct {
	InferenceMode           string `json:"inference_mode"`
	CUDARequired            bool   `json:"cuda_required"`
	CPUFallbackAllowed      bool   `json:"cpu_fallback_allowed"`
	CUDADeviceIndex         uint32 `json:"cuda_device_index"`
	TargetHardwareProfileID string `json:"target_hardware_profile_id"`
	OSRelease               string `json:"os_release"`
	Architecture            string `json:"architecture"`
	GPUVendor               string `json:"gpu_vendor"`
	GPUModel                string `json:"gpu_model"`
	GPUComputeCapability    string `json:"gpu_compute_capability"`
	GPUMemoryBytes          uint64 `json:"gpu_memory_bytes"`
	DriverVersion           string `json:"driver_version"`
	CUDAVersion             string `json:"cuda_version"`
	CUDNNVersion            string `json:"cudnn_version"`
	PythonVersion           string `json:"python_version"`
	ONNXRuntimeGPUVersion   string `json:"onnxruntime_gpu_version"`
	OpenCVVersion           string `json:"opencv_version"`
	TorchVersion            string `json:"torch_version"`
	TorchaudioVersion       string `json:"torchaudio_version"`
	CameraProfileID         string `json:"camera_profile_id"`
	MicrophoneProfileID     string `json:"microphone_profile_id"`
}

// ProtocolBindings describe all supported v1 preprocessing, score mapping,
// decision, clip, and enrollment rules explicitly.
type ProtocolBindings struct {
	FaceDetection      FaceDetectionProtocol      `json:"face_detection"`
	FaceIdentification FaceIdentificationProtocol `json:"face_identification"`
	FaceLiveness       FaceLivenessProtocol       `json:"face_liveness"`
	Speaker            SpeakerProtocol            `json:"speaker"`
}

type FaceDetectionProtocol struct {
	ProtocolID        string       `json:"protocol_id"`
	InputWidth        uint32       `json:"input_width"`
	InputHeight       uint32       `json:"input_height"`
	PixelFormat       string       `json:"pixel_format"`
	ResizeRule        string       `json:"resize_rule"`
	TensorRule        string       `json:"tensor_rule"`
	ScoreMapping      ScoreMapping `json:"score_mapping"`
	ScoreThresholdPPM uint32       `json:"score_threshold_ppm"`
	NMSRule           string       `json:"nms_rule"`
	NMSThresholdPPM   uint32       `json:"nms_threshold_ppm"`
	TopK              uint32       `json:"top_k"`
}

type FaceIdentificationProtocol struct {
	ProtocolID    string       `json:"protocol_id"`
	AlignmentRule string       `json:"alignment_rule"`
	PixelRule     string       `json:"pixel_rule"`
	EmbeddingRule string       `json:"embedding_rule"`
	ScoreMapping  ScoreMapping `json:"score_mapping"`
}

type FaceLivenessProtocol struct {
	ProtocolID        string       `json:"protocol_id"`
	CropRule          string       `json:"crop_rule"`
	InputRule         string       `json:"input_rule"`
	NormalizationRule string       `json:"normalization_rule"`
	ScoreMapping      ScoreMapping `json:"score_mapping"`
	RealOutputIndex   uint32       `json:"real_output_index"`
	SpoofOutputIndex  uint32       `json:"spoof_output_index"`
	DecisionRule      string       `json:"decision_rule"`
	ThresholdPPM      uint32       `json:"threshold_ppm"`
}

type SpeakerProtocol struct {
	ProtocolID                string       `json:"protocol_id"`
	PCMRule                   string       `json:"pcm_rule"`
	ClipRuleID                string       `json:"clip_rule_id"`
	MinimumClipUS             uint64       `json:"minimum_clip_us"`
	MaximumClipUS             uint64       `json:"maximum_clip_us"`
	MinimumVoicedUS           uint64       `json:"minimum_voiced_us"`
	MaximumLeadingSilenceUS   uint64       `json:"maximum_leading_silence_us"`
	MaximumTrailingSilenceUS  uint64       `json:"maximum_trailing_silence_us"`
	VADRuleID                 string       `json:"vad_rule_id"`
	FBankRuleID               string       `json:"fbank_rule_id"`
	EnrollmentMinimumClips    uint32       `json:"enrollment_minimum_clips"`
	EnrollmentMaximumClips    uint32       `json:"enrollment_maximum_clips"`
	EnrollmentAggregationRule string       `json:"enrollment_aggregation_rule"`
	ScoreMapping              ScoreMapping `json:"score_mapping"`
}

// ApplicationPolicy contains the only three identity resolver thresholds.
type ApplicationPolicy struct {
	FaceIdentificationThresholdPPM    uint32 `json:"face_identification_threshold_ppm"`
	SpeakerIdentificationThresholdPPM uint32 `json:"speaker_identification_threshold_ppm"`
	SpeakerVerificationThresholdPPM   uint32 `json:"speaker_verification_threshold_ppm"`
	RequireFaceLiveness               bool   `json:"require_face_liveness"`
}

// ProviderLatency binds one logical Provider to end-to-end measured latency.
type ProviderLatency struct {
	Capability          Capability `json:"capability"`
	ProviderID          string     `json:"provider_id"`
	SampleCount         uint64     `json:"sample_count"`
	QuantileRule        string     `json:"quantile_rule"`
	MeasurementBoundary string     `json:"measurement_boundary"`
	P95US               uint64     `json:"p95_us"`
	P99US               uint64     `json:"p99_us"`
	ObservedMaximumUS   uint64     `json:"observed_maximum_us"`
	DeclaredMaximumUS   uint64     `json:"declared_maximum_us"`
}

// Provenance binds the selected operating points to aggregate-only reports.
type Provenance struct {
	AggregateCurveReportSHA256   string `json:"aggregate_curve_report_sha256"`
	AggregateLatencyReportSHA256 string `json:"aggregate_latency_report_sha256"`
}
