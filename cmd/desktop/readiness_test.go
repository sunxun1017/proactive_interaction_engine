package main

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestActivationViewRequiresHealthyMediaAndSuppliesLocalDisplay(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	providers := &snapshotReader{}
	view, err := newActivationView(readiness.ScenarioRequirements{
		ID: "desktop",
		Required: []readiness.CapabilityRequirement{
			{Kind: readiness.PersonPresence, ProviderID: "desktop-presence"},
			{Kind: readiness.VoiceActivity, ProviderID: "desktop-vad"},
			{Kind: readiness.DisplayText, ProviderID: "web-avatar"},
		},
		Optional: []readiness.OptionalCapability{{Kind: readiness.SpeechSynthesis, ProviderID: "speech-dispatcher", Fallback: readiness.VisualOnly}},
	}, providers, clock, false)
	if err != nil {
		t.Fatalf("newActivationView() error = %v", err)
	}
	if got := view.CurrentActivation(); got.Status != readiness.Blocked {
		t.Fatalf("activation without media = %#v, want BLOCKED", got)
	}

	providers.snapshots = []readiness.ProviderSnapshot{
		validRuntimeProvider("desktop-presence", readiness.PersonPresence, now.Add(time.Minute)),
		validRuntimeProvider("desktop-vad", readiness.VoiceActivity, now.Add(time.Minute)),
	}
	if got := view.CurrentActivation(); got.Status != readiness.Degraded {
		t.Fatalf("activation with media and visual fallback = %#v, want DEGRADED", got)
	}

	withTTS, err := newActivationView(view.scenario, providers, clock, true)
	if err != nil {
		t.Fatalf("newActivationView(TTS) error = %v", err)
	}
	if got := withTTS.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("activation with media and TTS = %#v, want READY", got)
	}

	clock.Advance(time.Minute)
	if got := withTTS.CurrentActivation(); got.Status != readiness.Blocked {
		t.Fatalf("activation at media lease deadline = %#v, want BLOCKED", got)
	}
}

type snapshotReader struct{ snapshots []readiness.ProviderSnapshot }

func (s *snapshotReader) Snapshots() []readiness.ProviderSnapshot {
	return cloneProviderSnapshots(s.snapshots)
}

func validRuntimeProvider(id string, capability readiness.CapabilityKind, expiresAt time.Time) readiness.ProviderSnapshot {
	return readiness.ProviderSnapshot{
		ProviderID: id, InstanceID: id + "-1", ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{capability}, Health: readiness.Healthy, LeaseExpiresAt: expiresAt,
	}
}
