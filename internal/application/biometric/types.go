package biometric

import (
	"context"
	"time"

	"proactive-interaction-engine/internal/application/readiness"
)

// EnrollmentStatus is the durable lifecycle of an opaque template reference.
type EnrollmentStatus string

const (
	EnrollmentNone          EnrollmentStatus = "NONE"
	EnrollmentActive        EnrollmentStatus = "ACTIVE"
	EnrollmentDeletePending EnrollmentStatus = "DELETE_PENDING"
)

// Record combines one profile's consent and enrollment metadata for one
// independently revocable biometric capability.
type Record struct {
	ProfileRef          string
	Capability          readiness.CapabilityKind
	Consented           bool
	ConsentVersion      uint64
	ConsentUpdatedAt    time.Time
	TemplateRef         string
	ModelVersion        string
	Status              EnrollmentStatus
	EnrollmentUpdatedAt time.Time
	PendingStore        *PendingTemplateReference
	PendingDelete       *TemplateReference
}

// PendingTemplateReference durably records a prepared template store without
// allowing it to become active before the dedicated vault confirms storage.
// ConsentVersion prevents a prepared operation from surviving revocation and
// later re-authorization. StoreOperationID is a random, non-biometric binding
// to one vault write; it is never derived from template bytes.
type PendingTemplateReference struct {
	TemplateRef      string
	ModelVersion     string
	ConsentVersion   uint64
	StoreOperationID string
}

// TemplateReference is deletion metadata only; it never contains template
// bytes or model output.
type TemplateReference struct {
	TemplateRef  string
	ModelVersion string
}

// Snapshot is an immutable-by-convention, revisioned catalog view.
type Snapshot struct {
	Revision uint64
	Records  []Record
}

// Repository durably stores complete catalog snapshots with optimistic
// revision checks.
type Repository interface {
	Load(context.Context) (Snapshot, error)
	Save(context.Context, uint64, Snapshot) error
}

// ConsentCommand identifies one per-profile biometric consent.
type ConsentCommand struct {
	ProfileRef string
	Capability readiness.CapabilityKind
}

// Registration identifies one template lifecycle operation. Template bytes
// never enter this package.
type Registration struct {
	ProfileRef   string
	Capability   readiness.CapabilityKind
	TemplateRef  string
	ModelVersion string
}

// EnrollmentKey identifies one independently deletable enrollment.
type EnrollmentKey struct {
	ProfileRef string
	Capability readiness.CapabilityKind
}
