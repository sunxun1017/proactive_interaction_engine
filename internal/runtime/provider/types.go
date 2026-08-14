package provider

import "time"

// State is the lifecycle state of one explicitly configured provider process.
type State string

const (
	Disabled State = "DISABLED"
	Starting State = "STARTING"
	Running  State = "RUNNING"
	Degraded State = "DEGRADED"
	Stopping State = "STOPPING"
)

// Reason is a stable machine-readable runtime diagnostic.
type Reason string

const (
	ReasonNone                  Reason = "NONE"
	ReasonDisabledByUser        Reason = "DISABLED_BY_USER"
	ReasonPermissionDenied      Reason = "PERMISSION_DENIED"
	ReasonDeviceUnavailable     Reason = "DEVICE_UNAVAILABLE"
	ReasonDependencyUnavailable Reason = "DEPENDENCY_UNAVAILABLE"
	ReasonModelUnavailable      Reason = "MODEL_UNAVAILABLE"
	ReasonInternalError         Reason = "INTERNAL_ERROR"
	ReasonShuttingDown          Reason = "SHUTTING_DOWN"
)

// Runtime is the current state of one provider.
type Runtime struct {
	ProviderID string
	State      State
	Reason     Reason
	UpdatedAt  time.Time
}

// Snapshot is an immutable, revisioned provider-runtime view.
type Snapshot struct {
	Revision  uint64
	Providers []Runtime
}

// Source is the read-only provider-runtime view consumed by local UI adapters.
type Source interface {
	CurrentProviderRuntime() Snapshot
	SubscribeProviderRuntime() (Snapshot, <-chan Snapshot, func())
}
