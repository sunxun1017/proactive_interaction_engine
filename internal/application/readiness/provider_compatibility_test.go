package readiness

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestProviderMatchesCompatibilityReusesScenarioCompatibilityRules(t *testing.T) {
	compatibility := ProviderCompatibility{
		ProtocolVersion:              "v1",
		AllowedPrivacyClasses:        []ProviderPrivacyClass{ProviderPrivacyDeviceLocal},
		MaximumLatency:               time.Second,
		AllowedCancellationSemantics: []ProviderCancellationSemantics{ProviderCancellationCooperative},
		AllowedDeviceClasses:         []ProviderDeviceClass{ProviderDeviceCamera},
	}
	provider := ProviderSnapshot{
		ProviderID: "face", InstanceID: "face-instance", ProtocolVersion: "v1", ImplementationVersion: "face.v1",
		Capabilities: []CapabilityKind{FaceIdentification}, Health: Healthy, LeaseExpiresAt: time.Unix(1, 0),
		OperationalProfile: ProviderOperationalProfile{
			PrivacyClass: ProviderPrivacyDeviceLocal, MaximumLatency: 100 * time.Millisecond,
			CancellationSemantics: ProviderCancellationCooperative,
			DeviceRequirements:    []ProviderDeviceClass{ProviderDeviceCamera},
		},
	}
	matched, err := ProviderMatchesCompatibility(FaceIdentification, compatibility, provider)
	if err != nil || !matched {
		t.Fatalf("ProviderMatchesCompatibility(exact) = %t, %v, want true", matched, err)
	}

	tests := []struct {
		name   string
		mutate func(*ProviderSnapshot)
	}{
		{name: "protocol", mutate: func(value *ProviderSnapshot) { value.ProtocolVersion = "v2" }},
		{name: "privacy", mutate: func(value *ProviderSnapshot) { value.OperationalProfile.PrivacyClass = ProviderPrivacyRemoteProcessing }},
		{name: "latency", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.MaximumLatency = compatibility.MaximumLatency + time.Nanosecond
		}},
		{name: "cancellation", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.CancellationSemantics = ProviderCancellationBounded
		}},
		{name: "device", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.DeviceRequirements = []ProviderDeviceClass{ProviderDeviceMicrophone}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := provider
			candidate.Capabilities = append([]CapabilityKind(nil), provider.Capabilities...)
			candidate.OperationalProfile.DeviceRequirements = append([]ProviderDeviceClass(nil), provider.OperationalProfile.DeviceRequirements...)
			test.mutate(&candidate)
			matched, matchErr := ProviderMatchesCompatibility(FaceIdentification, compatibility, candidate)
			if matchErr != nil || matched {
				t.Fatalf("ProviderMatchesCompatibility(%s) = %t, %v, want false", test.name, matched, matchErr)
			}
		})
	}
}

func TestProviderMatchesCompatibilityRejectsInvalidInputs(t *testing.T) {
	provider := ProviderSnapshot{
		ProviderID: "face", InstanceID: "face-instance", ProtocolVersion: "v1", ImplementationVersion: "face.v1",
		Capabilities: []CapabilityKind{FaceIdentification}, Health: Healthy, LeaseExpiresAt: time.Unix(1, 0),
		OperationalProfile: ProviderOperationalProfile{
			PrivacyClass: ProviderPrivacyDeviceLocal, MaximumLatency: time.Millisecond,
			CancellationSemantics: ProviderCancellationCooperative,
		},
	}
	if _, err := ProviderMatchesCompatibility(FaceIdentification, ProviderCompatibility{}, provider); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ProviderMatchesCompatibility(invalid compatibility) error = %v, want InvalidInput", err)
	}
	compatibility := ProviderCompatibility{
		ProtocolVersion: "v1", AllowedPrivacyClasses: []ProviderPrivacyClass{ProviderPrivacyDeviceLocal}, MaximumLatency: time.Second,
		AllowedCancellationSemantics: []ProviderCancellationSemantics{ProviderCancellationCooperative},
	}
	provider.OperationalProfile.MaximumLatency = 0
	if _, err := ProviderMatchesCompatibility(FaceIdentification, compatibility, provider); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ProviderMatchesCompatibility(invalid provider) error = %v, want InvalidInput", err)
	}
}
