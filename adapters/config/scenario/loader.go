package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"

	"go.yaml.in/yaml/v3"
)

const (
	loadOp                 = "load scenario manifest"
	supportedSchemaVersion = "v1"
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

type wireManifest struct {
	SchemaVersion strictString  `yaml:"schema_version"`
	Scenario      *wireScenario `yaml:"scenario"`
}

type wireScenario struct {
	ID       strictString      `yaml:"id"`
	Version  strictString      `yaml:"version"`
	Required []wireRequirement `yaml:"required"`
	Optional []wireOptional    `yaml:"optional"`
}

type wireRequirement struct {
	Capability strictString `yaml:"capability"`
	ProviderID strictString `yaml:"provider_id"`
}

type wireOptional struct {
	Capability strictString `yaml:"capability"`
	ProviderID strictString `yaml:"provider_id"`
	Fallback   strictString `yaml:"fallback"`
}

type canonicalContent struct {
	SchemaVersion   string                 `json:"schema_version"`
	ScenarioID      string                 `json:"scenario_id"`
	ScenarioVersion string                 `json:"scenario_version"`
	Required        []canonicalRequirement `json:"required"`
	Optional        []canonicalOptional    `json:"optional"`
}

type canonicalRequirement struct {
	Capability string `json:"capability"`
	ProviderID string `json:"provider_id"`
}

type canonicalOptional struct {
	Capability string `json:"capability"`
	ProviderID string `json:"provider_id"`
	Fallback   string `json:"fallback"`
}

// Load decodes exactly one strict v1 YAML document from reader.
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
	requirements := readiness.ScenarioRequirements{
		ID: string(input.ID),
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
		requirements.Required = append(requirements.Required, readiness.CapabilityRequirement{
			Kind:       kind,
			ProviderID: string(item.ProviderID),
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
		requirements.Optional = append(requirements.Optional, readiness.OptionalCapability{
			Kind:       kind,
			ProviderID: string(item.ProviderID),
			Fallback:   fallback,
		})
	}
	return requirements, nil
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

func hashCanonical(schemaVersion, scenarioVersion string, requirements readiness.ScenarioRequirements) (string, error) {
	canonical := canonicalContent{
		SchemaVersion:   schemaVersion,
		ScenarioID:      requirements.ID,
		ScenarioVersion: scenarioVersion,
		Required:        make([]canonicalRequirement, 0, len(requirements.Required)),
		Optional:        make([]canonicalOptional, 0, len(requirements.Optional)),
	}
	for _, item := range requirements.Required {
		canonical.Required = append(canonical.Required, canonicalRequirement{
			Capability: string(item.Kind),
			ProviderID: item.ProviderID,
		})
	}
	for _, item := range requirements.Optional {
		canonical.Optional = append(canonical.Optional, canonicalOptional{
			Capability: string(item.Kind),
			ProviderID: item.ProviderID,
			Fallback:   string(item.Fallback),
		})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical manifest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func invalidInput(err error) error {
	return fault.New(fault.InvalidInput, loadOp, err)
}
