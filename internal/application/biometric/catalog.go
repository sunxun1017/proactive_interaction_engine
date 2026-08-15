package biometric

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

const catalogOp = "manage biometric catalog"

// ValidateSnapshot checks the complete catalog invariants without retaining or
// mutating the supplied snapshot.
func ValidateSnapshot(snapshot Snapshot) error {
	if _, err := canonicalize(snapshot); err != nil {
		return fault.New(fault.InvalidInput, "validate biometric catalog", err)
	}
	return nil
}

// Service serializes consent and enrollment metadata changes and publishes a
// new view only after durable persistence succeeds.
type Service struct {
	mu             sync.RWMutex
	repository     Repository
	clock          port.Clock
	current        Snapshot
	reloadRequired bool
}

// New restores and validates a biometric metadata catalog.
func New(ctx context.Context, repository Repository, clock port.Clock) (*Service, error) {
	const op = "create biometric catalog"
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if isNil(repository) || isNil(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("repository and clock are required"))
	}
	if err := ctx.Err(); err != nil {
		return nil, classify(op, err)
	}
	loaded, err := repository.Load(ctx)
	if err != nil {
		return nil, classify(op, err)
	}
	canonical, err := canonicalize(loaded)
	if err != nil {
		return nil, fault.New(fault.AdapterRejected, op, err)
	}
	return &Service{repository: repository, clock: clock, current: canonical}, nil
}

// Current returns a deep value copy in stable profile and capability order.
func (s *Service) Current() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.reloadRequired {
		return Snapshot{}
	}
	return cloneSnapshot(s.current)
}

// ReloadRequired reports whether an uncertain save outcome prevents the
// service from safely exposing or mutating its in-memory snapshot.
func (s *Service) ReloadRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reloadRequired
}

// Reload restores the latest durable snapshot after an uncertain repository
// save outcome. Until this succeeds all reads and mutations remain fail closed.
func (s *Service) Reload(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, fault.New(fault.InvalidInput, catalogOp, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, classify(catalogOp, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded, err := s.repository.Load(ctx)
	if err != nil {
		s.reloadRequired = true
		return Snapshot{}, classify(catalogOp, err)
	}
	canonical, err := canonicalize(loaded)
	if err != nil {
		s.reloadRequired = true
		return Snapshot{}, fault.New(fault.AdapterRejected, catalogOp, err)
	}
	s.current = canonical
	s.reloadRequired = false
	return cloneSnapshot(s.current), nil
}

// GrantConsent enables one profile and capability after durable persistence.
func (s *Service) GrantConsent(ctx context.Context, command ConsentCommand) (Snapshot, error) {
	if err := validateKey(command.ProfileRef, command.Capability); err != nil {
		return Snapshot{}, err
	}
	return s.change(ctx, func(next *Snapshot, now time.Time) (bool, error) {
		index := recordIndex(next.Records, command.ProfileRef, command.Capability)
		if index < 0 {
			next.Records = append(next.Records, Record{
				ProfileRef: command.ProfileRef, Capability: command.Capability,
				Consented: true, ConsentVersion: 1, ConsentUpdatedAt: now, Status: EnrollmentNone,
			})
			return true, nil
		}
		if next.Records[index].Consented {
			return false, nil
		}
		if err := advanceConsent(&next.Records[index], true, now); err != nil {
			return false, err
		}
		return true, nil
	})
}

// RevokeConsent disables authorization immediately without claiming that the
// separately stored template has already been deleted.
func (s *Service) RevokeConsent(ctx context.Context, command ConsentCommand) (Snapshot, error) {
	if err := validateKey(command.ProfileRef, command.Capability); err != nil {
		return Snapshot{}, err
	}
	return s.change(ctx, func(next *Snapshot, now time.Time) (bool, error) {
		index := recordIndex(next.Records, command.ProfileRef, command.Capability)
		if index < 0 || !next.Records[index].Consented {
			return false, nil
		}
		if err := advanceConsent(&next.Records[index], false, now); err != nil {
			return false, err
		}
		return true, nil
	})
}

// PrepareRegister durably stages a first template reference without making it
// usable. The dedicated vault adapter commits it only after encrypted storage.
func (s *Service) PrepareRegister(ctx context.Context, registration Registration, storeOperationID string) (Snapshot, error) {
	if err := validateRegistration(registration); err != nil {
		return Snapshot{}, err
	}
	if !validStoreOperationID(storeOperationID) {
		return Snapshot{}, invalidInput("store operation ID is invalid")
	}
	return s.change(ctx, func(next *Snapshot, _ time.Time) (bool, error) {
		index := recordIndex(next.Records, registration.ProfileRef, registration.Capability)
		if index < 0 || !next.Records[index].Consented {
			return false, fault.New(fault.PermissionDenied, catalogOp, errors.New("profile consent is required"))
		}
		record := &next.Records[index]
		if exactPendingStore(record.PendingStore, registration) {
			if record.PendingStore.StoreOperationID != storeOperationID {
				return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared store operation does not match"))
			}
			return false, nil
		}
		if record.PendingStore != nil {
			return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("another template store is pending"))
		}
		switch record.Status {
		case EnrollmentNone:
			if record.PendingDelete != nil {
				return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("previous template deletion is pending"))
			}
			record.PendingStore = pendingStore(registration, record.ConsentVersion, storeOperationID)
			return true, nil
		case EnrollmentActive:
			if record.TemplateRef == registration.TemplateRef && record.ModelVersion == registration.ModelVersion {
				return false, nil
			}
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("existing enrollment requires explicit replacement"))
		case EnrollmentDeletePending:
			return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("enrollment deletion is pending"))
		default:
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("unknown enrollment status"))
		}
	})
}

// PrepareReplace durably stages a replacement while the old active template
// remains usable until CommitPrepared succeeds.
func (s *Service) PrepareReplace(ctx context.Context, registration Registration, storeOperationID string) (Snapshot, error) {
	if err := validateRegistration(registration); err != nil {
		return Snapshot{}, err
	}
	if !validStoreOperationID(storeOperationID) {
		return Snapshot{}, invalidInput("store operation ID is invalid")
	}
	return s.change(ctx, func(next *Snapshot, _ time.Time) (bool, error) {
		index := recordIndex(next.Records, registration.ProfileRef, registration.Capability)
		if index < 0 || !next.Records[index].Consented {
			return false, fault.New(fault.PermissionDenied, catalogOp, errors.New("profile consent is required"))
		}
		record := &next.Records[index]
		if exactPendingStore(record.PendingStore, registration) {
			if record.PendingStore.StoreOperationID != storeOperationID {
				return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared store operation does not match"))
			}
			return false, nil
		}
		if record.PendingStore != nil {
			return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("another template store is pending"))
		}
		if record.Status == EnrollmentActive && record.TemplateRef == registration.TemplateRef && record.ModelVersion == registration.ModelVersion {
			return false, nil
		}
		if record.Status != EnrollmentActive {
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("an active enrollment is required for replacement"))
		}
		if record.PendingDelete != nil {
			return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("previous template deletion is pending"))
		}
		if record.TemplateRef == registration.TemplateRef {
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("replacement requires a new template reference"))
		}
		record.PendingStore = pendingStore(registration, record.ConsentVersion, storeOperationID)
		return true, nil
	})
}

// CommitPrepared activates one exact durably staged reference. A replacement
// moves the previous active reference into the retryable deletion queue.
func (s *Service) CommitPrepared(ctx context.Context, registration Registration, storeOperationID string) (Snapshot, error) {
	if err := validateRegistration(registration); err != nil {
		return Snapshot{}, err
	}
	if !validStoreOperationID(storeOperationID) {
		return Snapshot{}, invalidInput("store operation ID is invalid")
	}
	return s.change(ctx, func(next *Snapshot, now time.Time) (bool, error) {
		index := recordIndex(next.Records, registration.ProfileRef, registration.Capability)
		if index < 0 {
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared template store does not exist"))
		}
		record := &next.Records[index]
		if record.PendingStore == nil {
			if record.Status == EnrollmentActive && record.TemplateRef == registration.TemplateRef && record.ModelVersion == registration.ModelVersion {
				return false, nil
			}
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared template store does not exist"))
		}
		if !exactPendingStore(record.PendingStore, registration) {
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared template metadata does not match"))
		}
		if record.PendingStore.StoreOperationID != storeOperationID {
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared store operation does not match"))
		}
		if !record.Consented {
			return false, fault.New(fault.PermissionDenied, catalogOp, errors.New("profile consent was revoked after preparation"))
		}
		if record.ConsentVersion != record.PendingStore.ConsentVersion {
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("profile consent changed after preparation"))
		}
		switch record.Status {
		case EnrollmentNone:
		case EnrollmentActive:
			if record.PendingDelete != nil {
				return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("previous template deletion is pending"))
			}
			if record.TemplateRef == registration.TemplateRef {
				return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared replacement reuses the active template reference"))
			}
			record.PendingDelete = &TemplateReference{TemplateRef: record.TemplateRef, ModelVersion: record.ModelVersion}
		case EnrollmentDeletePending:
			return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("enrollment deletion is pending"))
		default:
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("unknown enrollment status"))
		}
		activate(record, registration, now)
		record.PendingStore = nil
		return true, nil
	})
}

// AbortPrepared clears one exact staged reference without changing the active
// enrollment. Repeating an already completed abort is idempotent.
func (s *Service) AbortPrepared(ctx context.Context, registration Registration, storeOperationID string) (Snapshot, error) {
	if err := validateRegistration(registration); err != nil {
		return Snapshot{}, err
	}
	if !validStoreOperationID(storeOperationID) {
		return Snapshot{}, invalidInput("store operation ID is invalid")
	}
	return s.change(ctx, func(next *Snapshot, _ time.Time) (bool, error) {
		index := recordIndex(next.Records, registration.ProfileRef, registration.Capability)
		if index < 0 || next.Records[index].PendingStore == nil {
			return false, nil
		}
		if !exactPendingStore(next.Records[index].PendingStore, registration) {
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared template metadata does not match"))
		}
		if next.Records[index].PendingStore.StoreOperationID != storeOperationID {
			return false, fault.New(fault.StaleInput, catalogOp, errors.New("prepared store operation does not match"))
		}
		next.Records[index].PendingStore = nil
		return true, nil
	})
}

// RequestDelete makes an enrollment unusable before a vault attempts physical
// deletion, leaving a durable reference that can be retried after restart.
func (s *Service) RequestDelete(ctx context.Context, key EnrollmentKey) (Snapshot, error) {
	if err := validateKey(key.ProfileRef, key.Capability); err != nil {
		return Snapshot{}, err
	}
	return s.change(ctx, func(next *Snapshot, now time.Time) (bool, error) {
		index := recordIndex(next.Records, key.ProfileRef, key.Capability)
		if index < 0 || next.Records[index].Status == EnrollmentNone || next.Records[index].Status == EnrollmentDeletePending {
			return false, nil
		}
		if next.Records[index].PendingStore != nil {
			return false, fault.New(fault.PolicyBlocked, catalogOp, errors.New("template store is pending"))
		}
		next.Records[index].Status = EnrollmentDeletePending
		next.Records[index].EnrollmentUpdatedAt = now
		return true, nil
	})
}

// ConfirmDelete removes a reference only after the vault confirms physical
// deletion. Repeating a confirmed deletion is idempotent.
func (s *Service) ConfirmDelete(ctx context.Context, key EnrollmentKey) (Snapshot, error) {
	if err := validateKey(key.ProfileRef, key.Capability); err != nil {
		return Snapshot{}, err
	}
	return s.change(ctx, func(next *Snapshot, _ time.Time) (bool, error) {
		index := recordIndex(next.Records, key.ProfileRef, key.Capability)
		if index < 0 || next.Records[index].Status == EnrollmentNone {
			return false, nil
		}
		if next.Records[index].Status != EnrollmentDeletePending {
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("enrollment deletion was not requested"))
		}
		record := &next.Records[index]
		record.TemplateRef = ""
		record.ModelVersion = ""
		record.Status = EnrollmentNone
		record.EnrollmentUpdatedAt = time.Time{}
		return true, nil
	})
}

// ConfirmRetiredDelete clears one replaced template reference only after the
// vault confirms that the retired template was physically deleted.
func (s *Service) ConfirmRetiredDelete(ctx context.Context, key EnrollmentKey, expected TemplateReference) (Snapshot, error) {
	if err := validateKey(key.ProfileRef, key.Capability); err != nil {
		return Snapshot{}, err
	}
	if !validString(expected.TemplateRef) || !validString(expected.ModelVersion) {
		return Snapshot{}, invalidInput("retired template reference and model version are required")
	}
	return s.change(ctx, func(next *Snapshot, _ time.Time) (bool, error) {
		index := recordIndex(next.Records, key.ProfileRef, key.Capability)
		if index < 0 {
			return false, nil
		}
		pending := next.Records[index].PendingDelete
		if pending == nil {
			return false, nil
		}
		if *pending != expected {
			return false, fault.New(fault.InvalidInput, catalogOp, errors.New("retired template metadata does not match pending deletion"))
		}
		next.Records[index].PendingDelete = nil
		return true, nil
	})
}

// DeleteProfile revokes every profile consent and moves all active template
// references to the recoverable deletion queue in one durable change.
func (s *Service) DeleteProfile(ctx context.Context, profileRef string) (Snapshot, error) {
	if !validString(profileRef) {
		return Snapshot{}, invalidInput("profile reference is required")
	}
	return s.change(ctx, func(next *Snapshot, now time.Time) (bool, error) {
		changed := false
		for index := range next.Records {
			record := &next.Records[index]
			if record.ProfileRef != profileRef {
				continue
			}
			if record.Consented {
				if err := advanceConsent(record, false, now); err != nil {
					return false, err
				}
				changed = true
			}
			if record.Status == EnrollmentActive {
				record.Status = EnrollmentDeletePending
				record.EnrollmentUpdatedAt = now
				changed = true
			}
		}
		return changed, nil
	})
}

// ReadinessPolicy intersects global device processing permission with one
// profile's consent and active enrollments.
func (s *Service) ReadinessPolicy(global privacy.Snapshot, profileRef string) (readiness.BiometricPolicySnapshot, error) {
	if !validString(profileRef) {
		return readiness.BiometricPolicySnapshot{}, invalidInput("profile reference is required")
	}
	globalEnabled, biometricRequested, err := biometricPermissions(global)
	if err != nil {
		return readiness.BiometricPolicySnapshot{}, err
	}
	s.mu.RLock()
	if s.reloadRequired {
		s.mu.RUnlock()
		return readiness.BiometricPolicySnapshot{}, fault.New(fault.Unavailable, catalogOp, errors.New("catalog reload is required"))
	}
	current := cloneSnapshot(s.current)
	s.mu.RUnlock()

	policy := readiness.BiometricPolicySnapshot{Enabled: biometricRequested}
	for _, capability := range biometricCapabilityOrder {
		if _, enabled := globalEnabled[capability]; !enabled {
			continue
		}
		if !requiresProfileConsent(capability) {
			policy.Authorized = append(policy.Authorized, capability)
			continue
		}
		index := recordIndex(current.Records, profileRef, capability)
		if index < 0 || !current.Records[index].Consented {
			continue
		}
		policy.Authorized = append(policy.Authorized, capability)
		if current.Records[index].Status == EnrollmentActive {
			policy.Enrolled = append(policy.Enrolled, capability)
		}
	}
	return policy, nil
}

func (s *Service) change(ctx context.Context, mutation func(*Snapshot, time.Time) (bool, error)) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, fault.New(fault.InvalidInput, catalogOp, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, classify(catalogOp, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reloadRequired {
		return Snapshot{}, fault.New(fault.Unavailable, catalogOp, errors.New("catalog reload is required"))
	}
	next := cloneSnapshot(s.current)
	now := s.clock.Now()
	if now.IsZero() {
		return Snapshot{}, fault.New(fault.InvalidInput, catalogOp, errors.New("clock returned zero time"))
	}
	changed, err := mutation(&next, now)
	if err != nil {
		return Snapshot{}, err
	}
	if !changed {
		return cloneSnapshot(s.current), nil
	}
	next.Revision++
	canonical, err := canonicalize(next)
	if err != nil {
		return Snapshot{}, fault.New(fault.InvalidInput, catalogOp, err)
	}
	next = canonical
	if err := s.repository.Save(ctx, s.current.Revision, cloneSnapshot(next)); err != nil {
		s.reloadRequired = true
		return Snapshot{}, classify(catalogOp, err)
	}
	s.current = next
	return cloneSnapshot(s.current), nil
}

func activate(record *Record, registration Registration, now time.Time) {
	record.TemplateRef = registration.TemplateRef
	record.ModelVersion = registration.ModelVersion
	record.Status = EnrollmentActive
	record.EnrollmentUpdatedAt = now
}

func pendingStore(registration Registration, consentVersion uint64, storeOperationID string) *PendingTemplateReference {
	return &PendingTemplateReference{
		TemplateRef: registration.TemplateRef, ModelVersion: registration.ModelVersion,
		ConsentVersion: consentVersion, StoreOperationID: storeOperationID,
	}
}

func exactPendingStore(pending *PendingTemplateReference, registration Registration) bool {
	return pending != nil && pending.TemplateRef == registration.TemplateRef && pending.ModelVersion == registration.ModelVersion
}

func validStoreOperationID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func advanceConsent(record *Record, consented bool, now time.Time) error {
	if record.ConsentVersion == ^uint64(0) {
		return fault.New(fault.PolicyBlocked, catalogOp, errors.New("consent version is exhausted"))
	}
	record.Consented = consented
	record.ConsentVersion++
	record.ConsentUpdatedAt = now
	return nil
}

func validateRegistration(registration Registration) error {
	if err := validateKey(registration.ProfileRef, registration.Capability); err != nil {
		return err
	}
	if !validString(registration.TemplateRef) || !validString(registration.ModelVersion) {
		return invalidInput("template reference and model version are required")
	}
	return nil
}

func validateKey(profileRef string, capability readiness.CapabilityKind) error {
	if !validString(profileRef) {
		return invalidInput("profile reference is required")
	}
	if !requiresProfileConsent(capability) {
		return invalidInput("capability %q does not support profile enrollment", capability)
	}
	return nil
}

func canonicalize(snapshot Snapshot) (Snapshot, error) {
	if snapshot.Revision == 0 && len(snapshot.Records) > 0 {
		return Snapshot{}, errors.New("revision zero cannot contain biometric records")
	}
	seen := make(map[string]struct{}, len(snapshot.Records))
	seenTemplates := make(map[string]struct{}, len(snapshot.Records)*2)
	canonical := cloneSnapshot(snapshot)
	for _, record := range canonical.Records {
		if err := validateKey(record.ProfileRef, record.Capability); err != nil {
			return Snapshot{}, err
		}
		key := record.ProfileRef + "\x00" + string(record.Capability)
		if _, exists := seen[key]; exists {
			return Snapshot{}, fmt.Errorf("biometric record %q/%q is duplicated", record.ProfileRef, record.Capability)
		}
		seen[key] = struct{}{}
		if record.ConsentUpdatedAt.IsZero() {
			return Snapshot{}, fmt.Errorf("biometric record %q/%q has no consent update time", record.ProfileRef, record.Capability)
		}
		if record.ConsentVersion == 0 {
			return Snapshot{}, fmt.Errorf("biometric record %q/%q has no consent version", record.ProfileRef, record.Capability)
		}
		switch record.Status {
		case EnrollmentNone:
			if record.TemplateRef != "" || record.ModelVersion != "" || !record.EnrollmentUpdatedAt.IsZero() {
				return Snapshot{}, errors.New("unenrolled biometric record contains template metadata")
			}
		case EnrollmentActive, EnrollmentDeletePending:
			if !validString(record.TemplateRef) || !validString(record.ModelVersion) || record.EnrollmentUpdatedAt.IsZero() {
				return Snapshot{}, errors.New("enrolled biometric record metadata is incomplete")
			}
			if _, duplicate := seenTemplates[record.TemplateRef]; duplicate {
				return Snapshot{}, errors.New("template reference is reused across biometric records")
			}
			seenTemplates[record.TemplateRef] = struct{}{}
		default:
			return Snapshot{}, fmt.Errorf("unknown enrollment status %q", record.Status)
		}
		if record.PendingDelete != nil {
			if !validString(record.PendingDelete.TemplateRef) || !validString(record.PendingDelete.ModelVersion) || record.PendingDelete.TemplateRef == record.TemplateRef {
				return Snapshot{}, errors.New("pending template deletion metadata is invalid")
			}
			if _, duplicate := seenTemplates[record.PendingDelete.TemplateRef]; duplicate {
				return Snapshot{}, errors.New("pending template reference is reused across biometric records")
			}
			seenTemplates[record.PendingDelete.TemplateRef] = struct{}{}
		}
		if record.PendingStore != nil {
			if !validString(record.PendingStore.TemplateRef) || !validString(record.PendingStore.ModelVersion) ||
				record.PendingStore.ConsentVersion == 0 || record.PendingStore.ConsentVersion > record.ConsentVersion ||
				!validStoreOperationID(record.PendingStore.StoreOperationID) || record.PendingStore.TemplateRef == record.TemplateRef {
				return Snapshot{}, errors.New("pending template store metadata is invalid")
			}
			if _, duplicate := seenTemplates[record.PendingStore.TemplateRef]; duplicate {
				return Snapshot{}, errors.New("pending template store reference is reused across biometric records")
			}
			seenTemplates[record.PendingStore.TemplateRef] = struct{}{}
		}
	}
	sortRecords(canonical.Records)
	return canonical, nil
}

var biometricCapabilityOrder = []readiness.CapabilityKind{
	readiness.FaceDetection,
	readiness.FaceIdentification,
	readiness.FaceLiveness,
	readiness.SpeakerIdentification,
	readiness.SpeakerVerification,
}

// CapabilityAuthorized reports whether every global privacy prerequisite for
// one biometric capability is granted. It does not inspect profile consent or
// enrollment state.
func CapabilityAuthorized(snapshot privacy.Snapshot, capability readiness.CapabilityKind) (bool, error) {
	biometric := false
	for _, candidate := range biometricCapabilityOrder {
		if candidate == capability {
			biometric = true
			break
		}
	}
	if !biometric {
		return false, invalidInput("capability %q is not biometric", capability)
	}

	effective, _, err := biometricPermissions(snapshot)
	if err != nil {
		return false, err
	}
	_, authorized := effective[capability]
	return authorized, nil
}

func biometricPermissions(snapshot privacy.Snapshot) (map[readiness.CapabilityKind]struct{}, bool, error) {
	if snapshot.Revision == 0 {
		for _, grant := range snapshot.Grants {
			if grant.Enabled || !grant.UpdatedAt.IsZero() {
				return nil, false, invalidInput("revision zero cannot contain changed global permissions")
			}
		}
	}
	known := make(map[privacy.Permission]struct{}, len(privacy.AllPermissions()))
	for _, permission := range privacy.AllPermissions() {
		known[permission] = struct{}{}
	}
	seen := make(map[privacy.Permission]struct{}, len(snapshot.Grants))
	granted := make(map[privacy.Permission]struct{})
	biometricRequested := false
	for _, grant := range snapshot.Grants {
		if _, ok := known[grant.Permission]; !ok {
			return nil, false, invalidInput("unknown global permission %q", grant.Permission)
		}
		if _, duplicate := seen[grant.Permission]; duplicate {
			return nil, false, invalidInput("global permission %q is duplicated", grant.Permission)
		}
		seen[grant.Permission] = struct{}{}
		if !grant.Enabled {
			continue
		}
		if grant.UpdatedAt.IsZero() {
			return nil, false, invalidInput("enabled global permission %q has no update time", grant.Permission)
		}
		granted[grant.Permission] = struct{}{}
		if _, ok := permissionCapability(grant.Permission); ok {
			biometricRequested = true
		}
	}

	effective := make(map[readiness.CapabilityKind]struct{})
	for _, capability := range biometricCapabilityOrder {
		if biometricPrerequisitesGranted(capability, granted) {
			effective[capability] = struct{}{}
		}
	}
	return effective, biometricRequested, nil
}

func biometricPrerequisitesGranted(capability readiness.CapabilityKind, granted map[privacy.Permission]struct{}) bool {
	var required []privacy.Permission
	switch capability {
	case readiness.FaceDetection:
		required = []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection}
	case readiness.FaceIdentification:
		required = []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification}
	case readiness.FaceLiveness:
		required = []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceLiveness}
	case readiness.SpeakerIdentification:
		required = []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerIdentification}
	case readiness.SpeakerVerification:
		required = []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerVerification}
	default:
		return false
	}
	for _, permission := range required {
		if _, ok := granted[permission]; !ok {
			return false
		}
	}
	return true
}

func permissionCapability(permission privacy.Permission) (readiness.CapabilityKind, bool) {
	switch permission {
	case privacy.FaceDetection:
		return readiness.FaceDetection, true
	case privacy.FaceIdentification:
		return readiness.FaceIdentification, true
	case privacy.FaceLiveness:
		return readiness.FaceLiveness, true
	case privacy.SpeakerIdentification:
		return readiness.SpeakerIdentification, true
	case privacy.SpeakerVerification:
		return readiness.SpeakerVerification, true
	default:
		return "", false
	}
}

func requiresProfileConsent(capability readiness.CapabilityKind) bool {
	switch capability {
	case readiness.FaceIdentification, readiness.SpeakerIdentification, readiness.SpeakerVerification:
		return true
	default:
		return false
	}
}

func recordIndex(records []Record, profileRef string, capability readiness.CapabilityKind) int {
	for index := range records {
		if records[index].ProfileRef == profileRef && records[index].Capability == capability {
			return index
		}
	}
	return -1
}

func sortRecords(records []Record) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].ProfileRef != records[j].ProfileRef {
			return records[i].ProfileRef < records[j].ProfileRef
		}
		return records[i].Capability < records[j].Capability
	})
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Records = append([]Record(nil), snapshot.Records...)
	for index := range snapshot.Records {
		if snapshot.Records[index].PendingStore != nil {
			pending := *snapshot.Records[index].PendingStore
			snapshot.Records[index].PendingStore = &pending
		}
		if snapshot.Records[index].PendingDelete != nil {
			pending := *snapshot.Records[index].PendingDelete
			snapshot.Records[index].PendingDelete = &pending
		}
	}
	return snapshot
}

func validString(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func invalidInput(format string, args ...any) error {
	return fault.New(fault.InvalidInput, catalogOp, fmt.Errorf(format, args...))
}

func classify(op string, err error) error {
	var typed *fault.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
