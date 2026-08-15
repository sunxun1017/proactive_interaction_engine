package scenario

import (
	"os"
	"reflect"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/readiness"
)

func TestAnonymousReturnWelcomeManifest(t *testing.T) {
	file, err := os.Open("../../../configs/scenarios/anonymous-return-welcome.v2.yaml")
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
		Version: "v2",
		Requirements: readiness.ScenarioRequirements{
			ID:                       "anonymous-return-welcome",
			MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous,
			Required: []readiness.CapabilityRequirement{
				{Kind: readiness.DisplayText, ProviderID: "web-avatar", Compatibility: manifestCompatibility(100*time.Millisecond, readiness.ProviderDeviceDisplay)},
				{Kind: readiness.PersonPresence, ProviderID: "desktop-presence", Compatibility: manifestCompatibility(3*time.Second, readiness.ProviderDeviceCamera)},
				{Kind: readiness.VoiceActivity, ProviderID: "desktop-vad", Compatibility: manifestCompatibility(time.Second, readiness.ProviderDeviceMicrophone)},
			},
			Optional: []readiness.OptionalCapability{
				{Kind: readiness.FaceDetection, ProviderID: "local-face-detection", Fallback: readiness.AnonymousSubject, Compatibility: manifestCompatibility(3*time.Second, readiness.ProviderDeviceCamera)},
				{Kind: readiness.FaceIdentification, ProviderID: "local-face-identity", Fallback: readiness.AnonymousSubject, Compatibility: manifestCompatibility(3*time.Second, readiness.ProviderDeviceCamera)},
				{Kind: readiness.FaceLiveness, ProviderID: "local-face-liveness", Fallback: readiness.AnonymousSubject, Compatibility: manifestCompatibility(3*time.Second, readiness.ProviderDeviceCamera)},
				{Kind: readiness.SpeakerIdentification, ProviderID: "local-speaker-identity", Fallback: readiness.AnonymousSubject, Compatibility: manifestCompatibility(3*time.Second, readiness.ProviderDeviceMicrophone)},
				{Kind: readiness.SpeechSynthesis, ProviderID: "speech-dispatcher", Fallback: readiness.VisualOnly, Compatibility: manifestCompatibility(2*time.Second, readiness.ProviderDeviceAudioOutput)},
			},
		},
	}
	if loaded.Version != want.Version || !reflect.DeepEqual(loaded.Requirements, want.Requirements) {
		t.Fatalf("Load(manifest) = %#v, want %#v", loaded, want)
	}
	assertSHA256Hex(t, loaded.Hash)
}

func manifestCompatibility(maximumLatency time.Duration, device readiness.ProviderDeviceClass) readiness.ProviderCompatibility {
	return readiness.ProviderCompatibility{
		ProtocolVersion:              "v1",
		AllowedPrivacyClasses:        []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal},
		MaximumLatency:               maximumLatency,
		AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
		AllowedDeviceClasses:         []readiness.ProviderDeviceClass{device},
	}
}
