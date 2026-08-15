package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"

	"go.yaml.in/yaml/v3"
)

const (
	loadOp                 = "load scenario manifest"
	supportedSchemaVersion = "v2"
)

// LoadedScenario is the validated canonical declaration and its reproducible
// content hash. Schema version is included in Hash but is not runtime policy.
type LoadedScenario struct {
	Version      string
	Requirements readiness.ScenarioRequirements
	Hash         string
}

type strictString string

func (value *strictString) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("value must be a YAML string")
	}
	*value = strictString(node.Value)
	return nil
}

type strictStringList struct {
	Present bool
	Values  []strictString
}

func (value *strictStringList) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.SequenceNode {
		return errors.New("value must be a YAML sequence")
	}
	value.Present = true
	value.Values = make([]strictString, 0, len(node.Content))
	for _, item := range node.Content {
		var decoded strictString
		if err := decoded.UnmarshalYAML(item); err != nil {
			return err
		}
		value.Values = append(value.Values, decoded)
	}
	return nil
}

type wireManifest struct {
	SchemaVersion strictString  `yaml:"schema_version"`
	Scenario      *wireScenario `yaml:"scenario"`
}

type wireScenario struct {
	ID                       strictString      `yaml:"id"`
	Version                  strictString      `yaml:"version"`
	MinimumIdentityAssurance strictString      `yaml:"minimum_identity_assurance"`
	Required                 []wireRequirement `yaml:"required"`
	Optional                 []wireOptional    `yaml:"optional"`
}

type wireRequirement struct {
	Capability    strictString       `yaml:"capability"`
	ProviderID    strictString       `yaml:"provider_id"`
	Compatibility *wireCompatibility `yaml:"compatibility"`
}

type wireOptional struct {
	Capability    strictString       `yaml:"capability"`
	ProviderID    strictString       `yaml:"provider_id"`
	Fallback      strictString       `yaml:"fallback"`
	Compatibility *wireCompatibility `yaml:"compatibility"`
}

type wireCompatibility struct {
	ProtocolVersion              strictString     `yaml:"protocol_version"`
	AllowedPrivacyClasses        strictStringList `yaml:"allowed_privacy_classes"`
	MaximumLatency               strictString     `yaml:"maximum_latency"`
	AllowedCancellationSemantics strictStringList `yaml:"allowed_cancellation_semantics"`
	AllowedDeviceClasses         strictStringList `yaml:"allowed_device_classes"`
}

type canonicalContent struct {
	SchemaVersion            string                 `json:"schema_version"`
	ScenarioID               string                 `json:"scenario_id"`
	ScenarioVersion          string                 `json:"scenario_version"`
	MinimumIdentityAssurance string                 `json:"minimum_identity_assurance"`
	Required                 []canonicalRequirement `json:"required"`
	Optional                 []canonicalOptional    `json:"optional"`
}

type canonicalRequirement struct {
	Capability    string                 `json:"capability"`
	ProviderID    string                 `json:"provider_id"`
	Compatibility canonicalCompatibility `json:"compatibility"`
}

type canonicalOptional struct {
	Capability    string                 `json:"capability"`
	ProviderID    string                 `json:"provider_id"`
	Fallback      string                 `json:"fallback"`
	Compatibility canonicalCompatibility `json:"compatibility"`
}

type canonicalCompatibility struct {
	ProtocolVersion              string   `json:"protocol_version"`
	AllowedPrivacyClasses        []string `json:"allowed_privacy_classes"`
	MaximumLatencyNanoseconds    int64    `json:"maximum_latency_nanoseconds"`
	AllowedCancellationSemantics []string `json:"allowed_cancellation_semantics"`
	AllowedDeviceClasses         []string `json:"allowed_device_classes"`
}

// Load decodes exactly one strict v2 YAML document from reader.
func Load(reader io.Reader) (LoadedScenario, error) {
	if reader == nil {
		return LoadedScenario{}, invalidInput(errors.New("reader is required"))
	}

	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var wire wireManifest
	if err := decoder.Decode(&wire); err != nil {
		return LoadedScenario{}, invalidInput(fmt.Errorf("decode manifest: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return LoadedScenario{}, invalidInput(fmt.Errorf("decode trailing document: %w", err))
		}
		return LoadedScenario{}, invalidInput(errors.New("multiple YAML documents are not allowed"))
	}

	if string(wire.SchemaVersion) != supportedSchemaVersion {
		return LoadedScenario{}, invalidInput(fmt.Errorf("unsupported schema version %q", wire.SchemaVersion))
	}
	if wire.Scenario == nil {
		return LoadedScenario{}, invalidInput(errors.New("scenario is required"))
	}
	if wire.Scenario.Version == "" {
		return LoadedScenario{}, invalidInput(errors.New("scenario version is required"))
	}

	requirements, err := mapRequirements(*wire.Scenario)
	if err != nil {
		return LoadedScenario{}, err
	}
	if err := readiness.ValidateScenario(requirements); err != nil {
		return LoadedScenario{}, invalidInput(err)
	}
	normalizeRequirements(&requirements)
	hash, err := hashCanonical(string(wire.SchemaVersion), string(wire.Scenario.Version), requirements)
	if err != nil {
		return LoadedScenario{}, invalidInput(err)
	}
	return LoadedScenario{
		Version:      string(wire.Scenario.Version),
		Requirements: requirements,
		Hash:         hash,
	}, nil
}

func mapRequirements(input wireScenario) (readiness.ScenarioRequirements, error) {
	assurance, ok := mapIdentityAssurance(string(input.MinimumIdentityAssurance))
	if !ok {
		return readiness.ScenarioRequirements{}, invalidInput(fmt.Errorf("unknown minimum identity assurance %q", input.MinimumIdentityAssurance))
	}
	requirements := readiness.ScenarioRequirements{
		ID:                       string(input.ID),
		MinimumIdentityAssurance: assurance,
	}
	if len(input.Required) > 0 {
		requirements.Required = make([]readiness.CapabilityRequirement, 0, len(input.Required))
	}
	if len(input.Optional) > 0 {
		requirements.Optional = make([]readiness.OptionalCapability, 0, len(input.Optional))
	}
	for _, item := range input.Required {
		kind, ok := mapCapability(string(item.Capability))
		if !ok {
			return readiness.ScenarioRequirements{}, invalidInput(fmt.Errorf("unknown capability %q", item.Capability))
		}
		compatibility, err := mapCompatibility(item.Compatibility)
		if err != nil {
			return readiness.ScenarioRequirements{}, err
		}
		requirements.Required = append(requirements.Required, readiness.CapabilityRequirement{
			Kind: kind, ProviderID: string(item.ProviderID), Compatibility: compatibility,
		})
	}
	for _, item := range input.Optional {
		kind, ok := mapCapability(string(item.Capability))
		if !ok {
			return readiness.ScenarioRequirements{}, invalidInput(fmt.Errorf("unknown capability %q", item.Capability))
		}
		fallback, ok := mapFallback(string(item.Fallback))
		if !ok {
			return readiness.ScenarioRequirements{}, invalidInput(fmt.Errorf("unknown fallback %q", item.Fallback))
		}
		compatibility, err := mapCompatibility(item.Compatibility)
		if err != nil {
			return readiness.ScenarioRequirements{}, err
		}
		requirements.Optional = append(requirements.Optional, readiness.OptionalCapability{
			Kind: kind, ProviderID: string(item.ProviderID), Fallback: fallback, Compatibility: compatibility,
		})
	}
	return requirements, nil
}

func mapIdentityAssurance(input string) (readiness.IdentityAssurance, bool) {
	switch input {
	case "ANONYMOUS":
		return readiness.IdentityAssuranceAnonymous, true
	case "RECOGNIZED":
		return readiness.IdentityAssuranceRecognized, true
	case "VERIFIED":
		return readiness.IdentityAssuranceVerified, true
	default:
		return "", false
	}
}

func mapCompatibility(input *wireCompatibility) (readiness.ProviderCompatibility, error) {
	if input == nil {
		return readiness.ProviderCompatibility{}, invalidInput(errors.New("compatibility is required for every capability selection"))
	}
	if !input.AllowedDeviceClasses.Present {
		return readiness.ProviderCompatibility{}, invalidInput(errors.New("allowed_device_classes is required in compatibility"))
	}
	maximumLatency, err := time.ParseDuration(string(input.MaximumLatency))
	if err != nil {
		return readiness.ProviderCompatibility{}, invalidInput(fmt.Errorf("parse maximum latency %q: %w", input.MaximumLatency, err))
	}
	privacyClasses := make([]readiness.ProviderPrivacyClass, 0, len(input.AllowedPrivacyClasses.Values))
	for _, value := range input.AllowedPrivacyClasses.Values {
		mapped, ok := mapPrivacyClass(string(value))
		if !ok {
			return readiness.ProviderCompatibility{}, invalidInput(fmt.Errorf("unknown privacy class %q", value))
		}
		privacyClasses = append(privacyClasses, mapped)
	}
	cancellation := make([]readiness.ProviderCancellationSemantics, 0, len(input.AllowedCancellationSemantics.Values))
	for _, value := range input.AllowedCancellationSemantics.Values {
		mapped, ok := mapCancellationSemantics(string(value))
		if !ok {
			return readiness.ProviderCompatibility{}, invalidInput(fmt.Errorf("unknown cancellation semantics %q", value))
		}
		cancellation = append(cancellation, mapped)
	}
	devices := make([]readiness.ProviderDeviceClass, 0, len(input.AllowedDeviceClasses.Values))
	for _, value := range input.AllowedDeviceClasses.Values {
		mapped, ok := mapDeviceClass(string(value))
		if !ok {
			return readiness.ProviderCompatibility{}, invalidInput(fmt.Errorf("unknown device class %q", value))
		}
		devices = append(devices, mapped)
	}
	return readiness.ProviderCompatibility{
		ProtocolVersion:              string(input.ProtocolVersion),
		AllowedPrivacyClasses:        privacyClasses,
		MaximumLatency:               maximumLatency,
		AllowedCancellationSemantics: cancellation,
		AllowedDeviceClasses:         devices,
	}, nil
}

func mapPrivacyClass(input string) (readiness.ProviderPrivacyClass, bool) {
	switch input {
	case "DEVICE_LOCAL":
		return readiness.ProviderPrivacyDeviceLocal, true
	case "REMOTE_PROCESSING":
		return readiness.ProviderPrivacyRemoteProcessing, true
	default:
		return "", false
	}
}

func mapCancellationSemantics(input string) (readiness.ProviderCancellationSemantics, bool) {
	switch input {
	case "NOT_SUPPORTED":
		return readiness.ProviderCancellationNotSupported, true
	case "COOPERATIVE":
		return readiness.ProviderCancellationCooperative, true
	case "BOUNDED":
		return readiness.ProviderCancellationBounded, true
	default:
		return "", false
	}
}

func mapDeviceClass(input string) (readiness.ProviderDeviceClass, bool) {
	switch input {
	case "CAMERA":
		return readiness.ProviderDeviceCamera, true
	case "MICROPHONE":
		return readiness.ProviderDeviceMicrophone, true
	case "DISPLAY":
		return readiness.ProviderDeviceDisplay, true
	case "AUDIO_OUTPUT":
		return readiness.ProviderDeviceAudioOutput, true
	case "EMBODIMENT_CONTROLLER":
		return readiness.ProviderDeviceEmbodimentController, true
	default:
		return "", false
	}
}

func mapCapability(input string) (readiness.CapabilityKind, bool) {
	switch input {
	case "PERSON_PRESENCE":
		return readiness.PersonPresence, true
	case "FACE_DETECTION":
		return readiness.FaceDetection, true
	case "FACE_IDENTIFICATION":
		return readiness.FaceIdentification, true
	case "FACE_LIVENESS":
		return readiness.FaceLiveness, true
	case "VOICE_ACTIVITY":
		return readiness.VoiceActivity, true
	case "SPEAKER_IDENTIFICATION":
		return readiness.SpeakerIdentification, true
	case "SPEAKER_VERIFICATION":
		return readiness.SpeakerVerification, true
	case "SPEECH_TRANSCRIPTION":
		return readiness.SpeechTranscription, true
	case "ATTENTION_ESTIMATION":
		return readiness.AttentionEstimation, true
	case "BUSY_STATE":
		return readiness.BusyState, true
	case "GESTURE_DETECTION":
		return readiness.GestureDetection, true
	case "DEVICE_STATE":
		return readiness.DeviceState, true
	case "DISPLAY_TEXT":
		return readiness.DisplayText, true
	case "AVATAR_ATTEND":
		return readiness.AvatarAttend, true
	case "AVATAR_EXPRESSION":
		return readiness.AvatarExpression, true
	case "SPEECH_SYNTHESIS":
		return readiness.SpeechSynthesis, true
	case "GESTURE":
		return readiness.Gesture, true
	case "LIGHT":
		return readiness.Light, true
	case "LOCOMOTION":
		return readiness.Locomotion, true
	default:
		return "", false
	}
}

func mapFallback(input string) (readiness.Fallback, bool) {
	switch input {
	case "ANONYMOUS_SUBJECT":
		return readiness.AnonymousSubject, true
	case "NO_VOICE_REPLY":
		return readiness.NoVoiceReply, true
	case "NO_TRANSCRIPT":
		return readiness.NoTranscript, true
	case "VISUAL_ONLY":
		return readiness.VisualOnly, true
	case "AUDIO_ONLY":
		return readiness.AudioOnly, true
	default:
		return "", false
	}
}

func normalizeRequirements(requirements *readiness.ScenarioRequirements) {
	for index := range requirements.Required {
		normalizeCompatibility(&requirements.Required[index].Compatibility)
	}
	for index := range requirements.Optional {
		normalizeCompatibility(&requirements.Optional[index].Compatibility)
	}
	sort.Slice(requirements.Required, func(i, j int) bool {
		if requirements.Required[i].Kind != requirements.Required[j].Kind {
			return requirements.Required[i].Kind < requirements.Required[j].Kind
		}
		return requirements.Required[i].ProviderID < requirements.Required[j].ProviderID
	})
	sort.Slice(requirements.Optional, func(i, j int) bool {
		if requirements.Optional[i].Kind != requirements.Optional[j].Kind {
			return requirements.Optional[i].Kind < requirements.Optional[j].Kind
		}
		if requirements.Optional[i].ProviderID != requirements.Optional[j].ProviderID {
			return requirements.Optional[i].ProviderID < requirements.Optional[j].ProviderID
		}
		return requirements.Optional[i].Fallback < requirements.Optional[j].Fallback
	})
}

func normalizeCompatibility(compatibility *readiness.ProviderCompatibility) {
	sort.Slice(compatibility.AllowedPrivacyClasses, func(i, j int) bool {
		return compatibility.AllowedPrivacyClasses[i] < compatibility.AllowedPrivacyClasses[j]
	})
	sort.Slice(compatibility.AllowedCancellationSemantics, func(i, j int) bool {
		return compatibility.AllowedCancellationSemantics[i] < compatibility.AllowedCancellationSemantics[j]
	})
	sort.Slice(compatibility.AllowedDeviceClasses, func(i, j int) bool {
		return compatibility.AllowedDeviceClasses[i] < compatibility.AllowedDeviceClasses[j]
	})
}

func hashCanonical(schemaVersion, scenarioVersion string, requirements readiness.ScenarioRequirements) (string, error) {
	canonical := canonicalContent{
		SchemaVersion:            schemaVersion,
		ScenarioID:               requirements.ID,
		ScenarioVersion:          scenarioVersion,
		MinimumIdentityAssurance: string(requirements.MinimumIdentityAssurance),
		Required:                 make([]canonicalRequirement, 0, len(requirements.Required)),
		Optional:                 make([]canonicalOptional, 0, len(requirements.Optional)),
	}
	for _, item := range requirements.Required {
		canonical.Required = append(canonical.Required, canonicalRequirement{
			Capability: string(item.Kind), ProviderID: item.ProviderID,
			Compatibility: canonicalizeCompatibility(item.Compatibility),
		})
	}
	for _, item := range requirements.Optional {
		canonical.Optional = append(canonical.Optional, canonicalOptional{
			Capability: string(item.Kind), ProviderID: item.ProviderID, Fallback: string(item.Fallback),
			Compatibility: canonicalizeCompatibility(item.Compatibility),
		})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical manifest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalizeCompatibility(input readiness.ProviderCompatibility) canonicalCompatibility {
	privacyClasses := make([]string, len(input.AllowedPrivacyClasses))
	for index, value := range input.AllowedPrivacyClasses {
		privacyClasses[index] = string(value)
	}
	cancellation := make([]string, len(input.AllowedCancellationSemantics))
	for index, value := range input.AllowedCancellationSemantics {
		cancellation[index] = string(value)
	}
	devices := make([]string, len(input.AllowedDeviceClasses))
	for index, value := range input.AllowedDeviceClasses {
		devices[index] = string(value)
	}
	return canonicalCompatibility{
		ProtocolVersion: input.ProtocolVersion, AllowedPrivacyClasses: privacyClasses,
		MaximumLatencyNanoseconds:    int64(input.MaximumLatency),
		AllowedCancellationSemantics: cancellation, AllowedDeviceClasses: devices,
	}
}

func invalidInput(err error) error {
	return fault.New(fault.InvalidInput, loadOp, err)
}
