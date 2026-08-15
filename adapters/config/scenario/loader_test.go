package scenario

import (
	"errors"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestLoadStrictV2Manifest(t *testing.T) {
	loaded := mustLoad(t, requiredManifest("anonymous-welcome", "v1", "ANONYMOUS", "PERSON_PRESENCE", "camera-main"))
	want := LoadedScenario{
		Version: "v1",
		Requirements: readiness.ScenarioRequirements{
			ID:                       "anonymous-welcome",
			MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous,
			Required: []readiness.CapabilityRequirement{{
				Kind: readiness.PersonPresence, ProviderID: "camera-main", Compatibility: defaultWireCompatibility(),
			}},
		},
	}
	if loaded.Version != want.Version || !reflect.DeepEqual(loaded.Requirements, want.Requirements) {
		t.Fatalf("Load() = %#v, want %#v", loaded, want)
	}
	assertSHA256Hex(t, loaded.Hash)

	legacy := strings.Replace(requiredManifest("legacy", "v1", "ANONYMOUS", "PERSON_PRESENCE", "camera"), "schema_version: v2", "schema_version: v1", 1)
	if _, err := Load(strings.NewReader(legacy)); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Load(v1) error = %v, want InvalidInput", err)
	}
}

func TestLoadMapsEveryCapabilityExplicitly(t *testing.T) {
	mappings := []struct {
		wire string
		app  readiness.CapabilityKind
	}{
		{"PERSON_PRESENCE", readiness.PersonPresence},
		{"FACE_DETECTION", readiness.FaceDetection},
		{"FACE_IDENTIFICATION", readiness.FaceIdentification},
		{"FACE_LIVENESS", readiness.FaceLiveness},
		{"VOICE_ACTIVITY", readiness.VoiceActivity},
		{"SPEAKER_IDENTIFICATION", readiness.SpeakerIdentification},
		{"SPEAKER_VERIFICATION", readiness.SpeakerVerification},
		{"SPEECH_TRANSCRIPTION", readiness.SpeechTranscription},
		{"ATTENTION_ESTIMATION", readiness.AttentionEstimation},
		{"BUSY_STATE", readiness.BusyState},
		{"GESTURE_DETECTION", readiness.GestureDetection},
		{"DEVICE_STATE", readiness.DeviceState},
		{"DISPLAY_TEXT", readiness.DisplayText},
		{"AVATAR_ATTEND", readiness.AvatarAttend},
		{"AVATAR_EXPRESSION", readiness.AvatarExpression},
		{"SPEECH_SYNTHESIS", readiness.SpeechSynthesis},
		{"GESTURE", readiness.Gesture},
		{"LIGHT", readiness.Light},
		{"LOCOMOTION", readiness.Locomotion},
	}

	var manifest strings.Builder
	manifest.WriteString("schema_version: v2\nscenario:\n  id: catalog\n  version: v1\n  minimum_identity_assurance: ANONYMOUS\n  required:\n")
	want := make([]readiness.CapabilityRequirement, 0, len(mappings))
	for _, mapping := range mappings {
		manifest.WriteString(requiredItem(mapping.wire, "provider-"+mapping.wire, defaultCompatibilityYAML))
		want = append(want, readiness.CapabilityRequirement{
			Kind: mapping.app, ProviderID: "provider-" + mapping.wire, Compatibility: defaultWireCompatibility(),
		})
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].Kind != want[j].Kind {
			return want[i].Kind < want[j].Kind
		}
		return want[i].ProviderID < want[j].ProviderID
	})

	loaded := mustLoad(t, manifest.String())
	if !reflect.DeepEqual(loaded.Requirements.Required, want) {
		t.Fatalf("Load(catalog) required = %#v, want %#v", loaded.Requirements.Required, want)
	}
}

func TestLoadMapsIdentityAssuranceAndCompatibilityExplicitly(t *testing.T) {
	manifest := "schema_version: v2\nscenario:\n  id: personalized\n  version: v3\n  minimum_identity_assurance: RECOGNIZED\n  optional:\n" +
		optionalItem("FACE_IDENTIFICATION", "face", "ANONYMOUS_SUBJECT", `      compatibility:
        protocol_version: protocol.v3
        allowed_privacy_classes: [REMOTE_PROCESSING, DEVICE_LOCAL]
        maximum_latency: 125ms
        allowed_cancellation_semantics: [BOUNDED, COOPERATIVE]
        allowed_device_classes: [MICROPHONE, CAMERA]
`)
	loaded := mustLoad(t, manifest)
	want := readiness.ScenarioRequirements{
		ID:                       "personalized",
		MinimumIdentityAssurance: readiness.IdentityAssuranceRecognized,
		Optional: []readiness.OptionalCapability{{
			Kind: readiness.FaceIdentification, ProviderID: "face", Fallback: readiness.AnonymousSubject,
			Compatibility: readiness.ProviderCompatibility{
				ProtocolVersion:              "protocol.v3",
				AllowedPrivacyClasses:        []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal, readiness.ProviderPrivacyRemoteProcessing},
				MaximumLatency:               125 * time.Millisecond,
				AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationBounded, readiness.ProviderCancellationCooperative},
				AllowedDeviceClasses:         []readiness.ProviderDeviceClass{readiness.ProviderDeviceCamera, readiness.ProviderDeviceMicrophone},
			},
		}},
	}
	if !reflect.DeepEqual(loaded.Requirements, want) {
		t.Fatalf("Load() requirements = %#v, want %#v", loaded.Requirements, want)
	}
}

func TestLoadRejectsMalformedOrInvalidV2Manifest(t *testing.T) {
	valid := requiredManifest("welcome", "v1", "ANONYMOUS", "PERSON_PRESENCE", "camera")
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "empty input"},
		{name: "malformed yaml", manifest: "schema_version: ["},
		{name: "legacy v1", manifest: strings.Replace(valid, "schema_version: v2", "schema_version: v1", 1)},
		{name: "unknown root field", manifest: valid + "unknown_root: true\n"},
		{name: "unknown scenario field", manifest: strings.Replace(valid, "  version: v1\n", "  version: v1\n  behavior: arbitrary\n", 1)},
		{name: "unknown compatibility field", manifest: strings.Replace(valid, "        protocol_version: v1\n", "        protocol_version: v1\n        model: forbidden\n", 1)},
		{name: "duplicate yaml key", manifest: strings.Replace(valid, "  id: welcome\n", "  id: welcome\n  id: duplicate\n", 1)},
		{name: "multiple documents", manifest: valid + "---\n" + valid},
		{name: "missing schema", manifest: strings.Replace(valid, "schema_version: v2\n", "", 1)},
		{name: "numeric schema", manifest: strings.Replace(valid, "schema_version: v2", "schema_version: 2", 1)},
		{name: "missing scenario", manifest: "schema_version: v2\n"},
		{name: "empty version", manifest: strings.Replace(valid, "  version: v1", "  version: ''", 1)},
		{name: "numeric version", manifest: strings.Replace(valid, "  version: v1", "  version: 1", 1)},
		{name: "empty id", manifest: strings.Replace(valid, "  id: welcome", "  id: ''", 1)},
		{name: "missing assurance", manifest: strings.Replace(valid, "  minimum_identity_assurance: ANONYMOUS\n", "", 1)},
		{name: "unknown assurance", manifest: strings.Replace(valid, "ANONYMOUS", "TRUSTED", 1)},
		{name: "numeric assurance", manifest: strings.Replace(valid, "ANONYMOUS", "7", 1)},
		{name: "no capabilities", manifest: "schema_version: v2\nscenario:\n  id: welcome\n  version: v1\n  minimum_identity_assurance: ANONYMOUS\n  required: []\n  optional: []\n"},
		{name: "unknown capability", manifest: strings.Replace(valid, "PERSON_PRESENCE", "UNKNOWN", 1)},
		{name: "numeric capability", manifest: strings.Replace(valid, "PERSON_PRESENCE", "17", 1)},
		{name: "empty provider", manifest: strings.Replace(valid, "provider_id: 'camera'", "provider_id: ''", 1)},
		{name: "missing compatibility", manifest: strings.Replace(valid, defaultCompatibilityYAML, "", 1)},
		{name: "missing protocol", manifest: strings.Replace(valid, "        protocol_version: v1\n", "", 1)},
		{name: "numeric protocol", manifest: strings.Replace(valid, "protocol_version: v1", "protocol_version: 1", 1)},
		{name: "missing privacy", manifest: strings.Replace(valid, "        allowed_privacy_classes: [DEVICE_LOCAL]\n", "", 1)},
		{name: "empty privacy", manifest: strings.Replace(valid, "[DEVICE_LOCAL]", "[]", 1)},
		{name: "unknown privacy", manifest: strings.Replace(valid, "DEVICE_LOCAL", "UNKNOWN", 1)},
		{name: "duplicate privacy", manifest: strings.Replace(valid, "[DEVICE_LOCAL]", "[DEVICE_LOCAL, DEVICE_LOCAL]", 1)},
		{name: "missing latency", manifest: strings.Replace(valid, "        maximum_latency: 250ms\n", "", 1)},
		{name: "numeric latency", manifest: strings.Replace(valid, "maximum_latency: 250ms", "maximum_latency: 250", 1)},
		{name: "zero latency", manifest: strings.Replace(valid, "250ms", "0s", 1)},
		{name: "invalid latency", manifest: strings.Replace(valid, "250ms", "soon", 1)},
		{name: "missing cancellation", manifest: strings.Replace(valid, "        allowed_cancellation_semantics: [COOPERATIVE]\n", "", 1)},
		{name: "empty cancellation", manifest: strings.Replace(valid, "[COOPERATIVE]", "[]", 1)},
		{name: "unknown cancellation", manifest: strings.Replace(valid, "COOPERATIVE", "UNKNOWN", 1)},
		{name: "duplicate cancellation", manifest: strings.Replace(valid, "[COOPERATIVE]", "[COOPERATIVE, COOPERATIVE]", 1)},
		{name: "missing devices", manifest: strings.Replace(valid, "        allowed_device_classes: [CAMERA]\n", "", 1)},
		{name: "unknown device", manifest: strings.Replace(valid, "CAMERA", "UNKNOWN", 1)},
		{name: "duplicate device", manifest: strings.Replace(valid, "[CAMERA]", "[CAMERA, CAMERA]", 1)},
		{name: "unknown fallback", manifest: optionalManifest("VOICE_ACTIVITY", "vad", "UNKNOWN")},
		{name: "mismatched fallback", manifest: optionalManifest("SPEECH_SYNTHESIS", "tts", "AUDIO_ONLY")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(test.manifest))
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Load() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestLoadCanonicalizesSelectionAndCompatibilityOrder(t *testing.T) {
	first := "schema_version: v2\nscenario:\n  id: welcome\n  version: v3\n  minimum_identity_assurance: ANONYMOUS\n  required:\n" +
		requiredItem("VOICE_ACTIVITY", "vad", orderedCompatibilityYAML) +
		requiredItem("PERSON_PRESENCE", "camera", reversedCompatibilityYAML) +
		"  optional:\n" + optionalItem("FACE_IDENTIFICATION", "face", "ANONYMOUS_SUBJECT", orderedCompatibilityYAML)
	second := "schema_version: v2\nscenario:\n  id: welcome\n  version: v3\n  minimum_identity_assurance: ANONYMOUS\n  required:\n" +
		requiredItem("PERSON_PRESENCE", "camera", orderedCompatibilityYAML) +
		requiredItem("VOICE_ACTIVITY", "vad", reversedCompatibilityYAML) +
		"  optional:\n" + optionalItem("FACE_IDENTIFICATION", "face", "ANONYMOUS_SUBJECT", reversedCompatibilityYAML)

	loadedFirst := mustLoad(t, first)
	loadedSecond := mustLoad(t, second)
	if !reflect.DeepEqual(loadedFirst, loadedSecond) {
		t.Fatalf("equivalent manifests differ: first %#v, second %#v", loadedFirst, loadedSecond)
	}
}

func TestLoadHashChangesWithV2SemanticValues(t *testing.T) {
	baselineManifest := requiredManifest("welcome", "v1", "ANONYMOUS", "PERSON_PRESENCE", "camera")
	baseline := mustLoad(t, baselineManifest)
	variants := []string{
		strings.Replace(baselineManifest, "id: welcome", "id: other", 1),
		strings.Replace(baselineManifest, "version: v1", "version: v2", 1),
		strings.Replace(baselineManifest, "ANONYMOUS", "RECOGNIZED", 1) + "  optional:\n" + optionalItem("FACE_IDENTIFICATION", "face", "ANONYMOUS_SUBJECT", defaultCompatibilityYAML),
		strings.Replace(baselineManifest, "provider_id: 'camera'", "provider_id: 'other-camera'", 1),
		strings.Replace(baselineManifest, "PERSON_PRESENCE", "DEVICE_STATE", 1),
		strings.Replace(baselineManifest, "protocol_version: v1", "protocol_version: v2", 1),
		strings.Replace(baselineManifest, "250ms", "300ms", 1),
	}
	for index, manifest := range variants {
		loaded := mustLoad(t, manifest)
		if loaded.Hash == baseline.Hash {
			t.Fatalf("semantic variant %d hash = baseline %q", index, baseline.Hash)
		}
	}
}

func TestLoadReaderFailureIsInvalidInput(t *testing.T) {
	_, err := Load(errorReader{err: errors.New("read failed")})
	if !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Load(error reader) error = %v, want InvalidInput", err)
	}
}

const defaultCompatibilityYAML = `      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [DEVICE_LOCAL]
        maximum_latency: 250ms
        allowed_cancellation_semantics: [COOPERATIVE]
        allowed_device_classes: [CAMERA]
`

const orderedCompatibilityYAML = `      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [DEVICE_LOCAL, REMOTE_PROCESSING]
        maximum_latency: 1s
        allowed_cancellation_semantics: [BOUNDED, COOPERATIVE]
        allowed_device_classes: [CAMERA, MICROPHONE]
`

const reversedCompatibilityYAML = `      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [REMOTE_PROCESSING, DEVICE_LOCAL]
        maximum_latency: 1000ms
        allowed_cancellation_semantics: [COOPERATIVE, BOUNDED]
        allowed_device_classes: [MICROPHONE, CAMERA]
`

func requiredManifest(id, version, assurance, capability, providerID string) string {
	return "schema_version: v2\nscenario:\n  id: " + id + "\n  version: " + version +
		"\n  minimum_identity_assurance: " + assurance + "\n  required:\n" +
		requiredItem(capability, providerID, defaultCompatibilityYAML)
}

func optionalManifest(capability, providerID, fallback string) string {
	return "schema_version: v2\nscenario:\n  id: optional\n  version: v1\n  minimum_identity_assurance: ANONYMOUS\n  optional:\n" +
		optionalItem(capability, providerID, fallback, defaultCompatibilityYAML)
}

func requiredItem(capability, providerID, compatibilityYAML string) string {
	return "    - capability: " + capability + "\n      provider_id: '" + providerID + "'\n" + compatibilityYAML
}

func optionalItem(capability, providerID, fallback, compatibilityYAML string) string {
	return "    - capability: " + capability + "\n      provider_id: '" + providerID + "'\n      fallback: " + fallback + "\n" + compatibilityYAML
}

func defaultWireCompatibility() readiness.ProviderCompatibility {
	return readiness.ProviderCompatibility{
		ProtocolVersion:              "v1",
		AllowedPrivacyClasses:        []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal},
		MaximumLatency:               250 * time.Millisecond,
		AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
		AllowedDeviceClasses:         []readiness.ProviderDeviceClass{readiness.ProviderDeviceCamera},
	}
}

func mustLoad(t *testing.T, manifest string) LoadedScenario {
	t.Helper()
	loaded, err := Load(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return loaded
}

func assertSHA256Hex(t *testing.T, hash string) {
	t.Helper()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		t.Fatalf("hash = %q, want 64 lowercase SHA-256 hex characters", hash)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = errorReader{}
