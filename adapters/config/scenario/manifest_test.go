package scenario

import (
	"os"
	"reflect"
	"testing"

	"proactive-interaction-engine/internal/application/readiness"
)

func TestAnonymousReturnWelcomeManifest(t *testing.T) {
	file, err := os.Open("../../../configs/scenarios/anonymous-return-welcome.v1.yaml")
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close manifest: %v", err)
		}
	}()

	loaded, err := Load(file)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := LoadedScenario{
		Version: "v1",
		Requirements: readiness.ScenarioRequirements{
			ID: "anonymous-return-welcome",
			Required: []readiness.CapabilityRequirement{
				{Kind: readiness.DisplayText, ProviderID: "web-avatar"},
				{Kind: readiness.PersonPresence, ProviderID: "desktop-presence"},
				{Kind: readiness.VoiceActivity, ProviderID: "desktop-vad"},
			},
			Optional: []readiness.OptionalCapability{
				{Kind: readiness.FaceIdentification, ProviderID: "local-face-identity", Fallback: readiness.AnonymousSubject},
				{Kind: readiness.SpeakerIdentification, ProviderID: "local-speaker-identity", Fallback: readiness.AnonymousSubject},
				{Kind: readiness.SpeechSynthesis, ProviderID: "speech-dispatcher", Fallback: readiness.VisualOnly},
			},
		},
	}
	if loaded.Version != want.Version || !reflect.DeepEqual(loaded.Requirements, want.Requirements) {
		t.Fatalf("Load(manifest) = %#v, want %#v", loaded, want)
	}
	assertSHA256Hex(t, loaded.Hash)
}
