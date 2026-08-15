package language

import (
	"errors"
	"fmt"
	"regexp"

	"proactive-interaction-engine/internal/domain/fault"
)

const routeOp = "route language request"

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Route applies hard gates and returns a pure, deterministic backend plan. It
// never inspects request content, calls a model, reads a clock, or mutates the
// runtime snapshot.
func Route(policy Policy, request Request, runtime RuntimeSnapshot) (Plan, error) {
	if err := policy.Validate(); err != nil {
		return Plan{}, err
	}
	if err := validateRequest(request); err != nil {
		return Plan{}, err
	}

	plan := func(primary Backend, reason Reason, fallbacks ...Backend) Plan {
		return Plan{
			PolicyVersion: policy.Version,
			Primary:       primary,
			Fallbacks:     append([]Backend(nil), fallbacks...),
			Reason:        reason,
		}
	}
	if request.Priority == PriorityP0 {
		return plan(BackendNone, ReasonP0Control), nil
	}
	switch request.Task {
	case TaskSilence:
		return plan(BackendNone, ReasonSilence), nil
	case TaskCancellation:
		return plan(BackendNone, ReasonCancellation), nil
	case TaskCriticalSystemPrompt:
		return plan(BackendTemplate, ReasonCriticalSystemTemplate), nil
	case TaskPhysicalAction, TaskPermissionChange:
		return plan(BackendTemplate, ReasonSafetyExplanation), nil
	case TaskUnknown:
		return plan(BackendTemplate, ReasonUnknownClarification), nil
	case TaskCasualConversation:
		eligible, reason, err := localEligible(policy.Local, request, runtime)
		if err != nil {
			return Plan{}, err
		}
		if eligible {
			return plan(BackendLocal, ReasonCasualLocal, BackendTemplate), nil
		}
		return plan(BackendTemplate, reason), nil
	case TaskExternalKnowledge:
		eligible, reason, err := cloudEligible(policy.Cloud, request, runtime)
		if err != nil {
			return Plan{}, err
		}
		if !eligible {
			return plan(BackendTemplate, reason), nil
		}
		return plan(BackendCloud, ReasonExternalCloud, BackendTemplate), nil
	default:
		return Plan{}, invalidRoute(fmt.Errorf("unknown task %q", request.Task))
	}
}

func localEligible(policy BackendPolicy, request Request, runtime RuntimeSnapshot) (bool, Reason, error) {
	if !policy.Enabled {
		return false, ReasonLocalDisabled, nil
	}
	if err := validateBackendRuntime("local", runtime.Local); err != nil {
		return false, "", err
	}
	if !runtime.LocalGPUAvailable {
		return false, ReasonLocalGPUUnavailable, nil
	}
	if runtime.Local.Health != BackendHealthy {
		return false, ReasonLocalUnhealthy, nil
	}
	if runtime.Local.Circuit != CircuitClosed {
		return false, ReasonLocalCircuitOpen, nil
	}
	if !runtime.Local.CapacityAvailable {
		return false, ReasonLocalCapacityExhausted, nil
	}
	if request.Remaining < policy.MinimumRemainingTime {
		return false, ReasonLocalDeadlineInsufficient, nil
	}
	if request.InputTokens > policy.MaxInputTokens || request.OutputTokens > policy.MaxOutputTokens {
		return false, ReasonLocalTokenLimit, nil
	}
	return true, ReasonCasualLocal, nil
}

func cloudEligible(policy CloudPolicy, request Request, runtime RuntimeSnapshot) (bool, Reason, error) {
	if !policy.Enabled {
		return false, ReasonCloudDisabled, nil
	}
	if !request.CloudAuthorized {
		return false, ReasonCloudPermissionMissing, nil
	}
	if request.Privacy != PrivacyPublic {
		return false, ReasonCloudPrivacyBlocked, nil
	}
	if err := validateBackendRuntime("cloud", runtime.Cloud); err != nil {
		return false, "", err
	}
	if runtime.CloudSessionTokensRemaining > policy.SessionTokenBudget {
		return false, "", invalidRoute(errors.New("cloud session remaining tokens exceed configured budget"))
	}
	if runtime.CloudDailyTokensRemaining > policy.DailyTokenBudget {
		return false, "", invalidRoute(errors.New("cloud daily remaining tokens exceed configured budget"))
	}
	if !runtime.NetworkAvailable {
		return false, ReasonCloudNetworkUnavailable, nil
	}
	if runtime.Cloud.Health != BackendHealthy {
		return false, ReasonCloudUnhealthy, nil
	}
	if runtime.Cloud.Circuit != CircuitClosed {
		return false, ReasonCloudCircuitOpen, nil
	}
	if !runtime.Cloud.CapacityAvailable {
		return false, ReasonCloudCapacityExhausted, nil
	}
	if request.Remaining < policy.MinimumRemainingTime {
		return false, ReasonCloudDeadlineInsufficient, nil
	}
	if request.InputTokens > policy.MaxInputTokens || request.OutputTokens > policy.MaxOutputTokens {
		return false, ReasonCloudTokenLimit, nil
	}
	requested := uint64(request.InputTokens) + uint64(request.OutputTokens)
	if requested > runtime.CloudSessionTokensRemaining || requested > runtime.CloudDailyTokensRemaining {
		return false, ReasonCloudBudgetExhausted, nil
	}
	return true, ReasonExternalCloud, nil
}

func validateRequest(request Request) error {
	if !requestIDPattern.MatchString(request.ID) {
		return invalidRoute(errors.New("request id must be a bounded identifier"))
	}
	if !validTask(request.Task) {
		return invalidRoute(fmt.Errorf("unknown task %q", request.Task))
	}
	if request.Privacy != PrivacyPublic && request.Privacy != PrivacyDevicePrivate && request.Privacy != PrivacySensitive {
		return invalidRoute(fmt.Errorf("unknown privacy class %q", request.Privacy))
	}
	if request.Priority != PriorityP0 && request.Priority != PriorityP1 && request.Priority != PriorityP2 && request.Priority != PriorityP3 {
		return invalidRoute(fmt.Errorf("unknown priority %q", request.Priority))
	}
	if request.Remaining < 0 {
		return invalidRoute(errors.New("remaining time must not be negative"))
	}
	return nil
}

func validTask(task TaskKind) bool {
	switch task {
	case TaskSilence, TaskCancellation, TaskCriticalSystemPrompt, TaskCasualConversation,
		TaskExternalKnowledge, TaskPhysicalAction, TaskPermissionChange, TaskUnknown:
		return true
	default:
		return false
	}
}

func validateBackendRuntime(name string, runtime BackendRuntime) error {
	if runtime.Health != BackendHealthy && runtime.Health != BackendUnhealthy {
		return invalidRoute(fmt.Errorf("%s backend has unknown health %q", name, runtime.Health))
	}
	if runtime.Circuit != CircuitClosed && runtime.Circuit != CircuitOpen {
		return invalidRoute(fmt.Errorf("%s backend has unknown circuit state %q", name, runtime.Circuit))
	}
	return nil
}

func invalidRoute(err error) error {
	return fault.New(fault.InvalidInput, routeOp, err)
}
