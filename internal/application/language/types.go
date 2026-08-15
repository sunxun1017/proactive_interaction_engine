package language

import "time"

// TaskKind is assigned by trusted application state. The router never infers
// it from user text.
type TaskKind string

const (
	TaskSilence              TaskKind = "SILENCE"
	TaskCancellation         TaskKind = "CANCELLATION"
	TaskCriticalSystemPrompt TaskKind = "CRITICAL_SYSTEM_PROMPT"
	TaskCasualConversation   TaskKind = "CASUAL_CONVERSATION"
	TaskExternalKnowledge    TaskKind = "EXTERNAL_KNOWLEDGE"
	TaskPhysicalAction       TaskKind = "PHYSICAL_ACTION"
	TaskPermissionChange     TaskKind = "PERMISSION_CHANGE"
	TaskUnknown              TaskKind = "UNKNOWN"
)

// PrivacyClass is the maximum disclosure allowed for one request.
type PrivacyClass string

const (
	PrivacyPublic        PrivacyClass = "PUBLIC"
	PrivacyDevicePrivate PrivacyClass = "DEVICE_PRIVATE"
	PrivacySensitive     PrivacyClass = "SENSITIVE"
)

// Priority follows the platform priority classes. P0 never invokes a language
// backend because cancellation and rejection must not wait for realization.
type Priority string

const (
	PriorityP0 Priority = "P0"
	PriorityP1 Priority = "P1"
	PriorityP2 Priority = "P2"
	PriorityP3 Priority = "P3"
)

// Backend is a closed set of language execution targets.
type Backend string

const (
	BackendNone     Backend = "NONE"
	BackendTemplate Backend = "TEMPLATE"
	BackendLocal    Backend = "LOCAL"
	BackendCloud    Backend = "CLOUD"
)

// Reason is the stable explanation for a routing plan.
type Reason string

const (
	ReasonP0Control                 Reason = "P0_CONTROL"
	ReasonSilence                   Reason = "SILENCE"
	ReasonCancellation              Reason = "CANCELLATION"
	ReasonCriticalSystemTemplate    Reason = "CRITICAL_SYSTEM_TEMPLATE"
	ReasonSafetyExplanation         Reason = "SAFETY_EXPLANATION"
	ReasonUnknownClarification      Reason = "UNKNOWN_CLARIFICATION"
	ReasonCasualLocal               Reason = "CASUAL_LOCAL"
	ReasonExternalCloud             Reason = "EXTERNAL_CLOUD"
	ReasonLocalDisabled             Reason = "LOCAL_DISABLED"
	ReasonLocalGPUUnavailable       Reason = "LOCAL_GPU_UNAVAILABLE"
	ReasonLocalUnhealthy            Reason = "LOCAL_UNHEALTHY"
	ReasonLocalCircuitOpen          Reason = "LOCAL_CIRCUIT_OPEN"
	ReasonLocalCapacityExhausted    Reason = "LOCAL_CAPACITY_EXHAUSTED"
	ReasonLocalDeadlineInsufficient Reason = "LOCAL_DEADLINE_INSUFFICIENT"
	ReasonLocalTokenLimit           Reason = "LOCAL_TOKEN_LIMIT"
	ReasonCloudDisabled             Reason = "CLOUD_DISABLED"
	ReasonCloudPermissionMissing    Reason = "CLOUD_PERMISSION_MISSING"
	ReasonCloudPrivacyBlocked       Reason = "CLOUD_PRIVACY_BLOCKED"
	ReasonCloudNetworkUnavailable   Reason = "CLOUD_NETWORK_UNAVAILABLE"
	ReasonCloudUnhealthy            Reason = "CLOUD_UNHEALTHY"
	ReasonCloudCircuitOpen          Reason = "CLOUD_CIRCUIT_OPEN"
	ReasonCloudCapacityExhausted    Reason = "CLOUD_CAPACITY_EXHAUSTED"
	ReasonCloudDeadlineInsufficient Reason = "CLOUD_DEADLINE_INSUFFICIENT"
	ReasonCloudTokenLimit           Reason = "CLOUD_TOKEN_LIMIT"
	ReasonCloudBudgetExhausted      Reason = "CLOUD_BUDGET_EXHAUSTED"
)

// Request contains only application-classified routing facts. It intentionally
// contains no prompt, transcript, private-memory value, or model object.
type Request struct {
	ID              string
	Task            TaskKind
	Privacy         PrivacyClass
	Priority        Priority
	CloudAuthorized bool
	Remaining       time.Duration
	InputTokens     uint32
	OutputTokens    uint32
}

// BackendHealth is the application-visible availability of one backend.
type BackendHealth string

const (
	BackendHealthy   BackendHealth = "HEALTHY"
	BackendUnhealthy BackendHealth = "UNHEALTHY"
)

// CircuitState is supplied by the runtime-owned circuit breaker. The pure
// router never mutates breaker state or performs a half-open probe.
type CircuitState string

const (
	CircuitClosed CircuitState = "CLOSED"
	CircuitOpen   CircuitState = "OPEN"
)

type BackendRuntime struct {
	Health            BackendHealth
	Circuit           CircuitState
	CapacityAvailable bool
}

// RuntimeSnapshot is an immutable routing input. Token values are remaining
// budgets, not cumulative usage.
type RuntimeSnapshot struct {
	Local                       BackendRuntime
	Cloud                       BackendRuntime
	LocalGPUAvailable           bool
	NetworkAvailable            bool
	CloudSessionTokensRemaining uint64
	CloudDailyTokensRemaining   uint64
}

// Plan is a deterministic primary route with an ordered, explicit fallback
// graph. Executors may only move from left to right and never invent a route.
type Plan struct {
	PolicyVersion string
	Primary       Backend
	Fallbacks     []Backend
	Reason        Reason
}

type BackendPolicy struct {
	Enabled              bool
	MinimumRemainingTime time.Duration
	MaxInputTokens       uint32
	MaxOutputTokens      uint32
}

type TemplatePolicy struct {
	Enabled bool
}

type CloudPolicy struct {
	BackendPolicy
	SessionTokenBudget uint64
	DailyTokenBudget   uint64
}

// Policy contains the complete, versioned routing configuration. It defines
// no defaults; adapters must load every field explicitly.
type Policy struct {
	Version  string
	Template TemplatePolicy
	Local    BackendPolicy
	Cloud    CloudPolicy
}
