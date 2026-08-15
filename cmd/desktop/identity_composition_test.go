package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"

	capabilityregistry "proactive-interaction-engine/adapters/capability/registry"
	"proactive-interaction-engine/adapters/storage/biometricvault"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestBuildIdentityDisabledHasNoIdentitySideEffectsOrRPC(t *testing.T) {
	config := testConfig(t)
	factoryCalls := 0
	app, err := build(config, func(biometricvault.SecretServiceMasterKeyConfig) (biometricvault.MasterKeyProvider, error) {
		factoryCalls++
		return nil, errors.New("identity factory must not be called")
	})
	if err != nil {
		t.Fatalf("build() error = %v", err)
	}
	defer app.workerTransport.Close()
	defer app.listener.Close()
	if factoryCalls != 0 || app.identity != nil {
		t.Fatalf("disabled identity side effects = calls %d composition %#v", factoryCalls, app.identity)
	}
	if _, registered := app.workerTransport.GRPC().GetServiceInfo()[platformv1.IdentityEvidenceIngressService_ServiceDesc.ServiceName]; registered {
		t.Fatal("identity evidence RPC registered while identity is disabled")
	}
}

func TestBuildIdentityEnabledUsesInjectedKeyAndRegistersRPC(t *testing.T) {
	config := testConfig(t)
	config.Identity = validIdentityConfig(t)
	key := fixedDesktopMasterKey(23)
	var received biometricvault.SecretServiceMasterKeyConfig
	factoryCalls := 0
	app, err := build(config, func(input biometricvault.SecretServiceMasterKeyConfig) (biometricvault.MasterKeyProvider, error) {
		factoryCalls++
		received = input
		return &desktopMasterKeyFake{key: key}, nil
	})
	if err != nil {
		t.Fatalf("build() error = %v", err)
	}
	defer app.workerTransport.Close()
	defer app.listener.Close()
	if factoryCalls != 1 {
		t.Fatalf("master-key factory calls = %d, want 1", factoryCalls)
	}
	if received.VaultRoot != config.Identity.VaultRoot || received.RuntimeDirectory != config.Identity.SecretServiceRuntimeDirectory {
		t.Fatalf("master-key config = %#v, want explicit identity paths", received)
	}
	if app.identity == nil || app.identity.catalog == nil || app.identity.vault == nil || app.identity.lifecycle == nil ||
		app.identity.runtime == nil || app.identity.ingress == nil || app.identity.readiness == nil {
		t.Fatalf("identity composition is incomplete: %#v", app.identity)
	}
	if _, registered := app.workerTransport.GRPC().GetServiceInfo()[platformv1.IdentityEvidenceIngressService_ServiceDesc.ServiceName]; !registered {
		t.Fatal("identity evidence RPC was not registered before Run")
	}
}

func TestDesktopIdentityCompositionRecoversPendingDeletionBeforeExposure(t *testing.T) {
	now := time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	config := validIdentityConfig(t)
	keyProvider := &desktopMasterKeyFake{key: fixedDesktopMasterKey(41)}
	seedPendingDesktopDeletion(t, config.VaultRoot, keyProvider, clock)

	permissions, err := privacy.New(context.Background(), &identityE2EPrivacyRepository{}, clock)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := capabilityregistry.New(clock, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	composition, err := newDesktopIdentityComposition(
		context.Background(), config, permissions, registry, clock, identityE2EScenario(),
		func(biometricvault.SecretServiceMasterKeyConfig) (biometricvault.MasterKeyProvider, error) {
			return keyProvider, nil
		},
	)
	if err != nil {
		t.Fatalf("newDesktopIdentityComposition() error = %v", err)
	}
	records := composition.catalog.Current().Records
	if len(records) != 1 || records[0].Status != biometric.EnrollmentNone || records[0].TemplateRef != "" {
		t.Fatalf("catalog after startup recovery = %#v", records)
	}
	descriptor := biometricvault.Descriptor{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-template-pending", ModelVersion: biometricvault.FaceInferenceProfileID,
	}
	if _, err := composition.vault.Open(context.Background(), descriptor); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(recovered template) error = %v, want Unavailable", err)
	}
}

func TestDesktopIdentityCompositionFailsClosedBeforeRuntime(t *testing.T) {
	now := time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	config := validIdentityConfig(t)
	permissions, err := privacy.New(context.Background(), &identityE2EPrivacyRepository{}, clock)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := capabilityregistry.New(clock, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := fault.New(fault.PermissionDenied, "fake Secret Service", errors.New("locked"))
	composition, err := newDesktopIdentityComposition(
		context.Background(), config, permissions, registry, clock, identityE2EScenario(),
		func(biometricvault.SecretServiceMasterKeyConfig) (biometricvault.MasterKeyProvider, error) {
			return &desktopMasterKeyFake{err: want}, nil
		},
	)
	if composition != nil || !fault.IsCode(err, fault.PermissionDenied) {
		t.Fatalf("newDesktopIdentityComposition() = %#v, %v; want nil PermissionDenied", composition, err)
	}
}

func TestDesktopIdentityCompositionFailsClosedWhenDeletionRecoveryFails(t *testing.T) {
	now := time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	config := validIdentityConfig(t)
	key := fixedDesktopMasterKey(47)
	seedPendingDesktopDeletion(t, config.VaultRoot, &desktopMasterKeyFake{key: key}, clock)

	permissions, err := privacy.New(context.Background(), &identityE2EPrivacyRepository{}, clock)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := capabilityregistry.New(clock, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	keys := &desktopSequencedMasterKeyFake{
		key:       key,
		err:       fault.New(fault.PermissionDenied, "fake Secret Service", errors.New("locked during recovery")),
		failAfter: 2,
	}
	composition, err := newDesktopIdentityComposition(
		context.Background(), config, permissions, registry, clock, identityE2EScenario(),
		func(biometricvault.SecretServiceMasterKeyConfig) (biometricvault.MasterKeyProvider, error) {
			return keys, nil
		},
	)
	if composition != nil || !fault.IsCode(err, fault.PermissionDenied) {
		t.Fatalf("newDesktopIdentityComposition() = %#v, %v; want nil PermissionDenied", composition, err)
	}
	if keys.calls != 3 {
		t.Fatalf("master-key calls = %d, want catalog load, lifecycle reload, then deletion recovery", keys.calls)
	}
}

func TestDesktopIdentityCompositionClassifiesCanceledStartup(t *testing.T) {
	config := validIdentityConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	composition, err := newDesktopIdentityComposition(ctx, config, nil, nil, nil, readiness.ScenarioRequirements{}, nil)
	if composition != nil || !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("newDesktopIdentityComposition(canceled) = %#v, %v; want nil Unavailable", composition, err)
	}
}

func seedPendingDesktopDeletion(t *testing.T, root string, keys *desktopMasterKeyFake, clock *engineclock.Fake) {
	t.Helper()
	ctx := context.Background()
	vault, err := biometricvault.New(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := biometricvault.NewCatalogRepository(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := biometric.New(ctx, repository, clock)
	if err != nil {
		t.Fatal(err)
	}
	registration := biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-template-pending", ModelVersion: biometricvault.FaceInferenceProfileID,
	}
	if _, err := catalog.GrantConsent(ctx, biometric.ConsentCommand{ProfileRef: registration.ProfileRef, Capability: registration.Capability}); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := biometricvault.NewTemplateLifecycleCoordinator(catalog, vault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Register(ctx, registration, validDesktopFaceTemplate(t)); err != nil {
		t.Fatal(err)
	}
	keys.err = fault.New(fault.PermissionDenied, "fake Secret Service", errors.New("locked while seeding pending delete"))
	keys.failAt = keys.calls + 2
	if _, err := lifecycle.Delete(ctx, biometric.EnrollmentKey{ProfileRef: registration.ProfileRef, Capability: registration.Capability}); !fault.IsCode(err, fault.PermissionDenied) {
		t.Fatalf("Delete() error = %v, want PermissionDenied", err)
	}
	keys.failAt = 0
	keys.err = nil
}

func validDesktopFaceTemplate(t *testing.T) []byte {
	t.Helper()
	digest, err := hex.DecodeString(biometricvault.FaceModelSHA256)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 50+128*4)
	copy(payload, []byte("PIEFACE1"))
	binary.LittleEndian.PutUint16(payload[8:10], 1)
	binary.LittleEndian.PutUint16(payload[10:12], 1)
	binary.LittleEndian.PutUint16(payload[12:14], 128)
	binary.LittleEndian.PutUint16(payload[14:16], 1)
	binary.LittleEndian.PutUint16(payload[16:18], 0x0101)
	copy(payload[18:50], digest)
	binary.LittleEndian.PutUint32(payload[50:54], math.Float32bits(1))
	return payload
}

type desktopMasterKeyFake struct {
	key    biometricvault.MasterKey
	err    error
	failAt int
	calls  int
}

type desktopSequencedMasterKeyFake struct {
	key       biometricvault.MasterKey
	err       error
	failAfter int
	calls     int
}

func (f *desktopSequencedMasterKeyFake) MasterKey(ctx context.Context) (biometricvault.MasterKey, error) {
	if err := ctx.Err(); err != nil {
		return biometricvault.MasterKey{}, err
	}
	f.calls++
	if f.calls > f.failAfter {
		return biometricvault.MasterKey{}, f.err
	}
	return f.key, nil
}

func (f *desktopMasterKeyFake) MasterKey(ctx context.Context) (biometricvault.MasterKey, error) {
	if err := ctx.Err(); err != nil {
		return biometricvault.MasterKey{}, err
	}
	f.calls++
	if f.failAt > 0 {
		if f.calls == f.failAt {
			return biometricvault.MasterKey{}, f.err
		}
		return f.key, nil
	}
	return f.key, f.err
}

func fixedDesktopMasterKey(value byte) biometricvault.MasterKey {
	var key biometricvault.MasterKey
	for index := range key {
		key[index] = value
	}
	return key
}
