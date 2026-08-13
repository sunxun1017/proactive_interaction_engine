package scenario

import (
	"errors"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestLoadMinimalAndFullManifest(t *testing.T) {
	minimal := mustLoad(t, `
schema_version: v1
scenario:
  id: anonymous-welcome
  version: v1
  required:
    - capability: PERSON_PRESENCE
      provider_id: camera-main
`)
	wantMinimal := LoadedScenario{
		Version: "v1",
		Requirements: readiness.ScenarioRequirements{
			ID:       "anonymous-welcome",
			Required: []readiness.CapabilityRequirement{{Kind: readiness.PersonPresence, ProviderID: "camera-main"}},
		},
	}
	if minimal.Version != wantMinimal.Version || !reflect.DeepEqual(minimal.Requirements, wantMinimal.Requirements) {
		t.Fatalf("Load(minimal) = %#v, want %#v", minimal, wantMinimal)
	}
	assertSHA256Hex(t, minimal.Hash)

	full := mustLoad(t, `
schema_version: v1
scenario:
  id: degraded-welcome
  version: welcome.v2
  required:
    - capability: PERSON_PRESENCE
      provider_id: camera-main
  optional:
    - capability: FACE_IDENTIFICATION
      provider_id: face-main
      fallback: ANONYMOUS_SUBJECT
    - capability: VOICE_ACTIVITY
      provider_id: vad-main
      fallback: NO_VOICE_REPLY
    - capability: SPEECH_TRANSCRIPTION
      provider_id: asr-main
      fallback: NO_TRANSCRIPT
    - capability: SPEECH_SYNTHESIS
      provider_id: tts-main
      fallback: VISUAL_ONLY
    - capability: DISPLAY_TEXT
      provider_id: avatar-main
      fallback: AUDIO_ONLY
`)
	wantOptional := []readiness.OptionalCapability{
		{Kind: readiness.DisplayText, ProviderID: "avatar-main", Fallback: readiness.AudioOnly},
		{Kind: readiness.FaceIdentification, ProviderID: "face-main", Fallback: readiness.AnonymousSubject},
		{Kind: readiness.SpeechSynthesis, ProviderID: "tts-main", Fallback: readiness.VisualOnly},
		{Kind: readiness.SpeechTranscription, ProviderID: "asr-main", Fallback: readiness.NoTranscript},
		{Kind: readiness.VoiceActivity, ProviderID: "vad-main", Fallback: readiness.NoVoiceReply},
	}
	if full.Version != "welcome.v2" || full.Requirements.ID != "degraded-welcome" || !reflect.DeepEqual(full.Requirements.Optional, wantOptional) {
		t.Fatalf("Load(full) = %#v, want normalized optional %#v", full, wantOptional)
	}
	assertSHA256Hex(t, full.Hash)
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
	manifest.WriteString("schema_version: v1\nscenario:\n  id: catalog\n  version: v1\n  required:\n")
	want := make([]readiness.CapabilityRequirement, 0, len(mappings))
	for index, mapping := range mappings {
		manifest.WriteString("    - capability: " + mapping.wire + "\n      provider_id: provider-" + mapping.wire + "\n")
		want = append(want, readiness.CapabilityRequirement{Kind: mapping.app, ProviderID: "provider-" + mapping.wire})
		if index > 0 && mapping.app == mappings[index-1].app {
			t.Fatalf("test capability mapping duplicates %q", mapping.app)
		}
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

func TestLoadRejectsMalformedOrInvalidManifest(t *testing.T) {
	valid := `
schema_version: v1
scenario:
  id: welcome
  version: v1
  required:
    - capability: PERSON_PRESENCE
      provider_id: camera
`
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "empty input"},
		{name: "malformed yaml", manifest: "schema_version: ["},
		{name: "unknown root field", manifest: valid + "unknown_root: true\n"},
		{name: "unknown scenario field", manifest: strings.Replace(valid, "  version: v1\n", "  version: v1\n  behavior: arbitrary\n", 1)},
		{name: "unknown required item field", manifest: strings.Replace(valid, "      provider_id: camera\n", "      provider_id: camera\n      model: forbidden\n", 1)},
		{name: "unknown optional item field", manifest: `
schema_version: v1
scenario:
  id: welcome
  version: v1
  optional:
    - capability: VOICE_ACTIVITY
      provider_id: vad
      fallback: NO_VOICE_REPLY
      attributes: forbidden
`},
		{name: "duplicate yaml key", manifest: strings.Replace(valid, "  id: welcome\n", "  id: welcome\n  id: duplicate\n", 1)},
		{name: "multiple documents", manifest: valid + "---\nschema_version: v1\nscenario:\n  id: second\n  version: v1\n  required:\n    - capability: PERSON_PRESENCE\n      provider_id: camera\n"},
		{name: "trailing nonempty document", manifest: valid + "---\nextra: value\n"},
		{name: "missing schema", manifest: strings.Replace(valid, "schema_version: v1\n", "", 1)},
		{name: "unknown schema", manifest: strings.Replace(valid, "schema_version: v1", "schema_version: v2", 1)},
		{name: "numeric schema", manifest: strings.Replace(valid, "schema_version: v1", "schema_version: 1", 1)},
		{name: "missing scenario", manifest: "schema_version: v1\n"},
		{name: "empty version", manifest: strings.Replace(valid, "  version: v1", "  version: ''", 1)},
		{name: "numeric version", manifest: strings.Replace(valid, "  version: v1", "  version: 1", 1)},
		{name: "boolean version", manifest: strings.Replace(valid, "  version: v1", "  version: false", 1)},
		{name: "empty id", manifest: strings.Replace(valid, "  id: welcome", "  id: ''", 1)},
		{name: "numeric id", manifest: strings.Replace(valid, "  id: welcome", "  id: 7", 1)},
		{name: "boolean id", manifest: strings.Replace(valid, "  id: welcome", "  id: true", 1)},
		{name: "no capabilities", manifest: "schema_version: v1\nscenario:\n  id: welcome\n  version: v1\n  required: []\n  optional: []\n"},
		{name: "unknown capability", manifest: strings.Replace(valid, "PERSON_PRESENCE", "UNKNOWN", 1)},
		{name: "numeric capability", manifest: strings.Replace(valid, "PERSON_PRESENCE", "17", 1)},
		{name: "unknown fallback", manifest: optionalManifest("VOICE_ACTIVITY", "vad", "UNKNOWN")},
		{name: "boolean fallback", manifest: optionalManifest("VOICE_ACTIVITY", "vad", "true")},
		{name: "mismatched fallback", manifest: optionalManifest("SPEECH_SYNTHESIS", "tts", "AUDIO_ONLY")},
		{name: "empty required provider", manifest: strings.Replace(valid, "provider_id: camera", "provider_id: ''", 1)},
		{name: "numeric required provider", manifest: strings.Replace(valid, "provider_id: camera", "provider_id: 42", 1)},
		{name: "empty optional provider", manifest: optionalManifest("VOICE_ACTIVITY", "", "NO_VOICE_REPLY")},
		{name: "duplicate required", manifest: strings.Replace(valid, "    - capability: PERSON_PRESENCE\n", "    - capability: PERSON_PRESENCE\n      provider_id: camera\n    - capability: PERSON_PRESENCE\n", 1)},
		{name: "duplicate optional", manifest: optionalManifest("VOICE_ACTIVITY", "vad", "NO_VOICE_REPLY") + "    - capability: VOICE_ACTIVITY\n      provider_id: vad\n      fallback: NO_VOICE_REPLY\n"},
		{name: "required optional overlap", manifest: valid + "  optional:\n    - capability: PERSON_PRESENCE\n      provider_id: camera\n      fallback: ANONYMOUS_SUBJECT\n"},
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

func TestLoadCanonicalizesOrderAndHash(t *testing.T) {
	first := mustLoad(t, `
schema_version: v1
scenario:
  id: welcome
  version: v3
  required:
    - capability: VOICE_ACTIVITY
      provider_id: vad
    - capability: PERSON_PRESENCE
      provider_id: camera
  optional:
    - capability: SPEECH_SYNTHESIS
      provider_id: tts
      fallback: VISUAL_ONLY
    - capability: FACE_IDENTIFICATION
      provider_id: face
      fallback: ANONYMOUS_SUBJECT
`)
	second := mustLoad(t, `
schema_version: v1
scenario:
  id: welcome
  version: v3
  required:
    - capability: PERSON_PRESENCE
      provider_id: camera
    - capability: VOICE_ACTIVITY
      provider_id: vad
  optional:
    - capability: FACE_IDENTIFICATION
      provider_id: face
      fallback: ANONYMOUS_SUBJECT
    - capability: SPEECH_SYNTHESIS
      provider_id: tts
      fallback: VISUAL_ONLY
`)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("equivalent manifests differ: first %#v, second %#v", first, second)
	}
	assertSHA256Hex(t, first.Hash)
}

func TestLoadHashChangesWithSemanticValues(t *testing.T) {
	baseline := mustLoad(t, canonicalManifest("welcome", "v1", "PERSON_PRESENCE", "camera"))
	variants := []string{
		canonicalManifest("other", "v1", "PERSON_PRESENCE", "camera"),
		canonicalManifest("welcome", "v2", "PERSON_PRESENCE", "camera"),
		canonicalManifest("welcome", "v1", "PERSON_PRESENCE", "other-camera"),
		canonicalManifest("welcome", "v1", "DEVICE_STATE", "camera"),
		canonicalManifest("welcome", "v1", "PERSON_PRESENCE", "camera") + "  optional:\n    - capability: VOICE_ACTIVITY\n      provider_id: vad\n      fallback: NO_VOICE_REPLY\n",
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

func mustLoad(t *testing.T, manifest string) LoadedScenario {
	t.Helper()
	loaded, err := Load(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return loaded
}

func optionalManifest(capability, providerID, fallback string) string {
	return "schema_version: v1\nscenario:\n  id: optional\n  version: v1\n  optional:\n" +
		"    - capability: " + capability + "\n      provider_id: '" + providerID + "'\n      fallback: " + fallback + "\n"
}

func canonicalManifest(id, version, capability, providerID string) string {
	return "schema_version: v1\nscenario:\n  id: " + id + "\n  version: " + version + "\n  required:\n" +
		"    - capability: " + capability + "\n      provider_id: " + providerID + "\n"
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
