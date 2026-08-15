package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	createDesktopIdentityRuntimeOp      = "create desktop identity runtime"
	advanceDesktopIdentificationOp      = "advance desktop identification"
	advanceDesktopSpeakerVerificationOp = "advance desktop speaker verification"
)

// identityPermissionReader supplies the latest authorization state only when a
// due identity window is closed.
type identityPermissionReader interface {
	Current(context.Context) (privacy.Snapshot, error)
}

// identityCatalogSnapshotReader supplies the latest biometric enrollment
// catalog only when a due identity window is closed.
type identityCatalogSnapshotReader interface {
	Current() biometric.Snapshot
}

// desktopIdentityRuntimeConfig contains every policy, lifetime, and capacity
// value used by the runtime. It deliberately defines no defaults.
type desktopIdentityRuntimeConfig struct {
	Policy         identity.Policy
	Identification identity.IdentificationCoordinatorConfig
	Verification   identity.SpeakerVerificationCoordinatorConfig
}

// desktopIdentityRuntime is a synchronous composition seam. Its caller owns
// all scheduling and invokes Advance methods with coordinator-issued wakeups.
type desktopIdentityRuntime struct {
	clock       port.Clock
	permissions identityPermissionReader
	catalog     identityCatalogSnapshotReader
	policy      identity.Policy

	identification *identity.EvidenceCoordinator
	verification   *identity.SpeakerVerificationCoordinator

	latestMu  sync.RWMutex
	latest    identity.Resolution
	hasLatest bool
}

func newDesktopIdentityRuntime(
	config desktopIdentityRuntimeConfig,
	permissions identityPermissionReader,
	catalog identityCatalogSnapshotReader,
	clock port.Clock,
) (*desktopIdentityRuntime, error) {
	if isNilDesktopIdentityDependency(clock) {
		return nil, desktopIdentityInvalid("clock is required")
	}
	if isNilDesktopIdentityDependency(permissions) {
		return nil, desktopIdentityInvalid("permission reader is required")
	}
	if isNilDesktopIdentityDependency(catalog) {
		return nil, desktopIdentityInvalid("catalog snapshot reader is required")
	}
	if err := identity.ValidateAt(config.Policy, identity.Evidence{}, clock.Now()); err != nil {
		return nil, fmt.Errorf("%s: validate policy: %w", createDesktopIdentityRuntimeOp, err)
	}
	if config.Identification.WindowDuration > config.Policy.MaxEvidenceAge {
		return nil, desktopIdentityInvalid("identification window duration must not exceed maximum evidence age")
	}
	if config.Verification.ChallengeDuration > config.Policy.MaxEvidenceAge {
		return nil, desktopIdentityInvalid("speaker verification challenge duration must not exceed maximum evidence age")
	}
	identification, err := identity.NewEvidenceCoordinator(config.Identification)
	if err != nil {
		return nil, fmt.Errorf("%s: configure identification: %w", createDesktopIdentityRuntimeOp, err)
	}
	verification, err := identity.NewSpeakerVerificationCoordinator(config.Verification)
	if err != nil {
		return nil, fmt.Errorf("%s: configure speaker verification: %w", createDesktopIdentityRuntimeOp, err)
	}
	return &desktopIdentityRuntime{
		clock:          clock,
		permissions:    permissions,
		catalog:        catalog,
		policy:         config.Policy,
		identification: identification,
		verification:   verification,
	}, nil
}

func (r *desktopIdentityRuntime) OpenIdentificationWindow(token string) (identity.IdentificationWindow, error) {
	return r.identification.OpenIdentificationWindow(token, r.clock.Now())
}

func (r *desktopIdentityRuntime) IssueSpeakerVerification(
	challengeID string,
	evidenceWindowID string,
	expectedProfileRef string,
) (identity.SpeakerVerificationChallenge, error) {
	return r.verification.IssueChallenge(challengeID, evidenceWindowID, expectedProfileRef, r.clock.Now())
}

func (r *desktopIdentityRuntime) AdvanceIdentification(
	ctx context.Context,
	wakeup identity.IdentificationWakeup,
) (identity.IdentificationAdvance, error) {
	if ctx == nil {
		return identity.IdentificationAdvance{}, fault.New(
			fault.InvalidInput,
			advanceDesktopIdentificationOp,
			errors.New("context is required"),
		)
	}
	if err := ctx.Err(); err != nil {
		return identity.IdentificationAdvance{}, classifyDesktopIdentityFailure(advanceDesktopIdentificationOp, err)
	}
	now := r.clock.Now()
	if now.Before(wakeup.Deadline) {
		return r.identification.AdvanceAt(
			wakeup,
			r.policy,
			privacy.Snapshot{},
			biometric.Snapshot{},
			now,
		)
	}
	permissions, enrollments, err := r.currentIdentitySnapshots(ctx, advanceDesktopIdentificationOp)
	if err != nil {
		return identity.IdentificationAdvance{}, err
	}
	result, err := r.identification.AdvanceAt(wakeup, r.policy, permissions, enrollments, now)
	if err != nil {
		return identity.IdentificationAdvance{}, err
	}
	if result.Resolved {
		r.recordLatest(result.Resolution)
	}
	return result, nil
}

func (r *desktopIdentityRuntime) AdvanceVerification(
	ctx context.Context,
	wakeup identity.SpeakerVerificationWakeup,
) (identity.SpeakerVerificationAdvance, error) {
	if ctx == nil {
		return identity.SpeakerVerificationAdvance{}, fault.New(
			fault.InvalidInput,
			advanceDesktopSpeakerVerificationOp,
			errors.New("context is required"),
		)
	}
	if err := ctx.Err(); err != nil {
		return identity.SpeakerVerificationAdvance{}, classifyDesktopIdentityFailure(advanceDesktopSpeakerVerificationOp, err)
	}
	now := r.clock.Now()
	if now.Before(wakeup.Deadline) {
		return r.verification.AdvanceAt(
			wakeup,
			r.policy,
			privacy.Snapshot{},
			biometric.Snapshot{},
			now,
		)
	}
	permissions, enrollments, err := r.currentIdentitySnapshots(ctx, advanceDesktopSpeakerVerificationOp)
	if err != nil {
		return identity.SpeakerVerificationAdvance{}, err
	}
	result, err := r.verification.AdvanceAt(wakeup, r.policy, permissions, enrollments, now)
	if err != nil {
		return identity.SpeakerVerificationAdvance{}, err
	}
	if result.Resolved {
		r.recordLatest(result.Resolution)
	}
	return result, nil
}

func (r *desktopIdentityRuntime) LatestResolution() (identity.Resolution, bool) {
	r.latestMu.RLock()
	defer r.latestMu.RUnlock()
	return r.latest, r.hasLatest
}

func (r *desktopIdentityRuntime) currentIdentitySnapshots(
	ctx context.Context,
	op string,
) (privacy.Snapshot, biometric.Snapshot, error) {
	permissions, err := r.permissions.Current(ctx)
	if err != nil {
		return privacy.Snapshot{}, biometric.Snapshot{}, classifyDesktopIdentityFailure(op, err)
	}
	return permissions, r.catalog.Current(), nil
}

func (r *desktopIdentityRuntime) recordLatest(resolution identity.Resolution) {
	r.latestMu.Lock()
	defer r.latestMu.Unlock()
	r.latest = resolution
	r.hasLatest = true
}

func desktopIdentityInvalid(message string) error {
	return fault.New(fault.InvalidInput, createDesktopIdentityRuntimeOp, errors.New(message))
}

func classifyDesktopIdentityFailure(op string, err error) error {
	var typed *fault.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func isNilDesktopIdentityDependency(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}
