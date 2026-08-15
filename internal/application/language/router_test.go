package language

import (
	"reflect"
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestRouteHardGatesNeverInvokeModels(t *testing.T) {
	policy := testPolicy()
	runtime := testRuntime()
	tests := []struct {
		name    string
		request Request
		want    Plan
	}{
		{
			name:    "P0 control",
			request: testRequest(TaskCasualConversation, PrivacyPublic, PriorityP0),
			want:    Plan{PolicyVersion: policy.Version, Primary: BackendNone, Reason: ReasonP0Control},
		},
		{
			name:    "silence",
			request: testRequest(TaskSilence, PrivacyPublic, PriorityP2),
			want:    Plan{PolicyVersion: policy.Version, Primary: BackendNone, Reason: ReasonSilence},
		},
		{
			name:    "cancel",
			request: testRequest(TaskCancellation, PrivacyPublic, PriorityP1),
			want:    Plan{PolicyVersion: policy.Version, Primary: BackendNone, Reason: ReasonCancellation},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Route(policy, test.request, runtime)
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Route() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRouteSafetyAndSystemTasksUseReviewedTemplates(t *testing.T) {
	policy := testPolicy()
	runtime := testRuntime()
	for _, task := range []TaskKind{
		TaskCriticalSystemPrompt,
		TaskPhysicalAction,
		TaskPermissionChange,
		TaskUnknown,
	} {
		t.Run(string(task), func(t *testing.T) {
			got, err := Route(policy, testRequest(task, PrivacyPublic, PriorityP1), runtime)
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if got.Primary != BackendTemplate || len(got.Fallbacks) != 0 {
				t.Fatalf("Route() = %#v, want template only", got)
			}
			if task == TaskPhysicalAction || task == TaskPermissionChange {
				if got.Reason != ReasonSafetyExplanation {
					t.Fatalf("Reason = %q, want %q", got.Reason, ReasonSafetyExplanation)
				}
			}
		})
	}
}

func TestRouteCasualConversationUsesLocalOnlyWithTemplateFallback(t *testing.T) {
	policy := testPolicy()
	runtime := testRuntime()
	request := testRequest(TaskCasualConversation, PrivacyDevicePrivate, PriorityP2)
	request.CloudAuthorized = true

	got, err := Route(policy, request, runtime)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	want := Plan{
		PolicyVersion: policy.Version,
		Primary:       BackendLocal,
		Fallbacks:     []Backend{BackendTemplate},
		Reason:        ReasonCasualLocal,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Route() = %#v, want %#v", got, want)
	}
	for _, backend := range append([]Backend{got.Primary}, got.Fallbacks...) {
		if backend == BackendCloud {
			t.Fatal("local route silently escalated to cloud")
		}
	}
}

func TestRouteCasualConversationFallsBackForEveryLocalGate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Policy, *Request, *RuntimeSnapshot)
		reason Reason
	}{
		{name: "disabled", mutate: func(p *Policy, _ *Request, _ *RuntimeSnapshot) { p.Local.Enabled = false }, reason: ReasonLocalDisabled},
		{name: "GPU unavailable", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.LocalGPUAvailable = false }, reason: ReasonLocalGPUUnavailable},
		{name: "unhealthy", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Local.Health = BackendUnhealthy }, reason: ReasonLocalUnhealthy},
		{name: "circuit open", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Local.Circuit = CircuitOpen }, reason: ReasonLocalCircuitOpen},
		{name: "capacity exhausted", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Local.CapacityAvailable = false }, reason: ReasonLocalCapacityExhausted},
		{name: "deadline below minimum", mutate: func(p *Policy, r *Request, _ *RuntimeSnapshot) {
			r.Remaining = p.Local.MinimumRemainingTime - time.Nanosecond
		}, reason: ReasonLocalDeadlineInsufficient},
		{name: "input token limit", mutate: func(p *Policy, r *Request, _ *RuntimeSnapshot) { r.InputTokens = p.Local.MaxInputTokens + 1 }, reason: ReasonLocalTokenLimit},
		{name: "output token limit", mutate: func(p *Policy, r *Request, _ *RuntimeSnapshot) { r.OutputTokens = p.Local.MaxOutputTokens + 1 }, reason: ReasonLocalTokenLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy()
			request := testRequest(TaskCasualConversation, PrivacyPublic, PriorityP2)
			runtime := testRuntime()
			test.mutate(&policy, &request, &runtime)
			got, err := Route(policy, request, runtime)
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if got.Primary != BackendTemplate || len(got.Fallbacks) != 0 || got.Reason != test.reason {
				t.Fatalf("Route() = %#v, want template/%s", got, test.reason)
			}
		})
	}
}

func TestRouteExternalKnowledgeRequiresEveryCloudGate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Policy, *Request, *RuntimeSnapshot)
		reason Reason
	}{
		{name: "disabled", mutate: func(p *Policy, _ *Request, _ *RuntimeSnapshot) { p.Cloud.Enabled = false }, reason: ReasonCloudDisabled},
		{name: "request permission missing", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.CloudAuthorized = false }, reason: ReasonCloudPermissionMissing},
		{name: "device private", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.Privacy = PrivacyDevicePrivate }, reason: ReasonCloudPrivacyBlocked},
		{name: "sensitive", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.Privacy = PrivacySensitive }, reason: ReasonCloudPrivacyBlocked},
		{name: "network unavailable", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.NetworkAvailable = false }, reason: ReasonCloudNetworkUnavailable},
		{name: "unhealthy", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Cloud.Health = BackendUnhealthy }, reason: ReasonCloudUnhealthy},
		{name: "circuit open", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Cloud.Circuit = CircuitOpen }, reason: ReasonCloudCircuitOpen},
		{name: "capacity exhausted", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Cloud.CapacityAvailable = false }, reason: ReasonCloudCapacityExhausted},
		{name: "deadline below minimum", mutate: func(p *Policy, r *Request, _ *RuntimeSnapshot) {
			r.Remaining = p.Cloud.MinimumRemainingTime - time.Nanosecond
		}, reason: ReasonCloudDeadlineInsufficient},
		{name: "input token limit", mutate: func(p *Policy, r *Request, _ *RuntimeSnapshot) { r.InputTokens = p.Cloud.MaxInputTokens + 1 }, reason: ReasonCloudTokenLimit},
		{name: "output token limit", mutate: func(p *Policy, r *Request, _ *RuntimeSnapshot) { r.OutputTokens = p.Cloud.MaxOutputTokens + 1 }, reason: ReasonCloudTokenLimit},
		{name: "session budget", mutate: func(_ *Policy, r *Request, s *RuntimeSnapshot) {
			s.CloudSessionTokensRemaining = uint64(r.InputTokens+r.OutputTokens) - 1
		}, reason: ReasonCloudBudgetExhausted},
		{name: "daily budget", mutate: func(_ *Policy, r *Request, s *RuntimeSnapshot) {
			s.CloudDailyTokensRemaining = uint64(r.InputTokens+r.OutputTokens) - 1
		}, reason: ReasonCloudBudgetExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy()
			request := testRequest(TaskExternalKnowledge, PrivacyPublic, PriorityP2)
			request.CloudAuthorized = true
			runtime := testRuntime()
			test.mutate(&policy, &request, &runtime)
			got, err := Route(policy, request, runtime)
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if got.Primary != BackendTemplate || len(got.Fallbacks) != 0 || got.Reason != test.reason {
				t.Fatalf("Route() = %#v, want template/%s", got, test.reason)
			}
		})
	}
}

func TestRouteExternalKnowledgeUsesCloudWithExplicitFallbackGraph(t *testing.T) {
	policy := testPolicy()
	request := testRequest(TaskExternalKnowledge, PrivacyPublic, PriorityP2)
	request.CloudAuthorized = true
	runtime := testRuntime()

	got, err := Route(policy, request, runtime)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	want := Plan{
		PolicyVersion: policy.Version,
		Primary:       BackendCloud,
		Fallbacks:     []Backend{BackendTemplate},
		Reason:        ReasonExternalCloud,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Route() = %#v, want %#v", got, want)
	}
}

func TestRouteAcceptsExactDeadlineAndCloudBudgetBoundaries(t *testing.T) {
	policy := testPolicy()
	runtime := testRuntime()

	localRequest := testRequest(TaskCasualConversation, PrivacySensitive, PriorityP2)
	localRequest.Remaining = policy.Local.MinimumRemainingTime
	localRequest.InputTokens = policy.Local.MaxInputTokens
	localRequest.OutputTokens = policy.Local.MaxOutputTokens
	localPlan, err := Route(policy, localRequest, runtime)
	if err != nil {
		t.Fatalf("Route(local boundary) error = %v", err)
	}
	if localPlan.Primary != BackendLocal {
		t.Fatalf("Route(local boundary) = %#v, want local", localPlan)
	}

	cloudRequest := testRequest(TaskExternalKnowledge, PrivacyPublic, PriorityP2)
	cloudRequest.CloudAuthorized = true
	cloudRequest.Remaining = policy.Cloud.MinimumRemainingTime
	cloudRequest.InputTokens = policy.Cloud.MaxInputTokens
	cloudRequest.OutputTokens = policy.Cloud.MaxOutputTokens
	requested := uint64(cloudRequest.InputTokens) + uint64(cloudRequest.OutputTokens)
	runtime.CloudSessionTokensRemaining = requested
	runtime.CloudDailyTokensRemaining = requested
	cloudPlan, err := Route(policy, cloudRequest, runtime)
	if err != nil {
		t.Fatalf("Route(cloud boundary) error = %v", err)
	}
	if cloudPlan.Primary != BackendCloud {
		t.Fatalf("Route(cloud boundary) = %#v, want cloud", cloudPlan)
	}
}

func TestRouteIsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	policy := testPolicy()
	request := testRequest(TaskExternalKnowledge, PrivacyPublic, PriorityP2)
	request.CloudAuthorized = true
	runtime := testRuntime()

	first, err := Route(policy, request, runtime)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	first.Fallbacks[0] = BackendNone
	second, err := Route(policy, request, runtime)
	if err != nil {
		t.Fatalf("Route() second error = %v", err)
	}
	if !reflect.DeepEqual(second.Fallbacks, []Backend{BackendTemplate}) {
		t.Fatalf("Route() second fallbacks = %#v", second.Fallbacks)
	}
}

func TestRouteRuntimeIndependentPathsIgnoreMalformedBackendSnapshot(t *testing.T) {
	policy := testPolicy()
	runtime := RuntimeSnapshot{
		Local:                       BackendRuntime{Health: "INVALID", Circuit: "INVALID"},
		Cloud:                       BackendRuntime{Health: "INVALID", Circuit: "INVALID"},
		CloudSessionTokensRemaining: policy.Cloud.SessionTokenBudget + 1,
		CloudDailyTokensRemaining:   policy.Cloud.DailyTokenBudget + 1,
	}
	tests := []struct {
		name    string
		request Request
		backend Backend
	}{
		{name: "P0", request: testRequest(TaskCasualConversation, PrivacyPublic, PriorityP0), backend: BackendNone},
		{name: "silence", request: testRequest(TaskSilence, PrivacyPublic, PriorityP2), backend: BackendNone},
		{name: "cancel", request: testRequest(TaskCancellation, PrivacyPublic, PriorityP1), backend: BackendNone},
		{name: "critical template", request: testRequest(TaskCriticalSystemPrompt, PrivacyPublic, PriorityP1), backend: BackendTemplate},
		{name: "physical explanation", request: testRequest(TaskPhysicalAction, PrivacyPublic, PriorityP1), backend: BackendTemplate},
		{name: "permission explanation", request: testRequest(TaskPermissionChange, PrivacySensitive, PriorityP1), backend: BackendTemplate},
		{name: "unknown clarification", request: testRequest(TaskUnknown, PrivacyDevicePrivate, PriorityP2), backend: BackendTemplate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("Route() panicked: %v", recovered)
				}
			}()
			plan, err := Route(policy, test.request, runtime)
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if plan.Primary != test.backend {
				t.Fatalf("Route() = %#v, want %s", plan, test.backend)
			}
		})
	}
}

func TestRouteTemplateOnlyPolicyAcceptsZeroRuntimeSnapshot(t *testing.T) {
	policy := testPolicy()
	policy.Local.Enabled = false
	policy.Cloud.Enabled = false
	tests := []struct {
		task    TaskKind
		backend Backend
	}{
		{task: TaskSilence, backend: BackendNone},
		{task: TaskCancellation, backend: BackendNone},
		{task: TaskCriticalSystemPrompt, backend: BackendTemplate},
		{task: TaskCasualConversation, backend: BackendTemplate},
		{task: TaskExternalKnowledge, backend: BackendTemplate},
		{task: TaskPhysicalAction, backend: BackendTemplate},
		{task: TaskPermissionChange, backend: BackendTemplate},
		{task: TaskUnknown, backend: BackendTemplate},
	}
	for _, test := range tests {
		t.Run(string(test.task), func(t *testing.T) {
			request := testRequest(test.task, PrivacyPublic, PriorityP2)
			request.CloudAuthorized = true
			plan, err := Route(policy, request, RuntimeSnapshot{})
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if plan.Primary != test.backend {
				t.Fatalf("Route() = %#v, want %s", plan, test.backend)
			}
		})
	}
}

func TestRouteValidatesOnlyTheBackendItMayUse(t *testing.T) {
	policy := testPolicy()

	badCloud := testRuntime()
	badCloud.Cloud = BackendRuntime{Health: "INVALID", Circuit: "INVALID"}
	badCloud.CloudSessionTokensRemaining = policy.Cloud.SessionTokenBudget + 1
	localPlan, err := Route(policy, testRequest(TaskCasualConversation, PrivacyPublic, PriorityP2), badCloud)
	if err != nil || localPlan.Primary != BackendLocal {
		t.Fatalf("Route(casual with bad cloud) = %#v, %v, want local", localPlan, err)
	}

	badLocal := testRuntime()
	badLocal.Local = BackendRuntime{Health: "INVALID", Circuit: "INVALID"}
	cloudRequest := testRequest(TaskExternalKnowledge, PrivacyPublic, PriorityP2)
	cloudRequest.CloudAuthorized = true
	cloudPlan, err := Route(policy, cloudRequest, badLocal)
	if err != nil || cloudPlan.Primary != BackendCloud {
		t.Fatalf("Route(external with bad local) = %#v, %v, want cloud", cloudPlan, err)
	}

	badCloudRequest := cloudRequest
	badCloudRequest.CloudAuthorized = false
	templatePlan, err := Route(policy, badCloudRequest, badCloud)
	if err != nil || templatePlan.Primary != BackendTemplate || templatePlan.Reason != ReasonCloudPermissionMissing {
		t.Fatalf("Route(unauthorized with bad cloud) = %#v, %v, want permission template", templatePlan, err)
	}

	privateCloudRequest := cloudRequest
	privateCloudRequest.Privacy = PrivacySensitive
	templatePlan, err = Route(policy, privateCloudRequest, badCloud)
	if err != nil || templatePlan.Primary != BackendTemplate || templatePlan.Reason != ReasonCloudPrivacyBlocked {
		t.Fatalf("Route(private with bad cloud) = %#v, %v, want privacy template", templatePlan, err)
	}
}

func TestRouteRejectsInvalidTypedInputs(t *testing.T) {
	policy := testPolicy()
	request := testRequest(TaskCasualConversation, PrivacyPublic, PriorityP2)
	runtime := testRuntime()
	tests := []struct {
		name   string
		mutate func(*Policy, *Request, *RuntimeSnapshot)
	}{
		{name: "policy", mutate: func(p *Policy, _ *Request, _ *RuntimeSnapshot) { p.Version = "" }},
		{name: "task", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.Task = "invented" }},
		{name: "privacy", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.Privacy = "invented" }},
		{name: "priority", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.Priority = "invented" }},
		{name: "remaining", mutate: func(_ *Policy, r *Request, _ *RuntimeSnapshot) { r.Remaining = -time.Nanosecond }},
		{name: "local health", mutate: func(_ *Policy, _ *Request, s *RuntimeSnapshot) { s.Local.Health = "invented" }},
		{name: "cloud circuit", mutate: func(_ *Policy, r *Request, s *RuntimeSnapshot) {
			r.Task = TaskExternalKnowledge
			r.CloudAuthorized = true
			s.Cloud.Circuit = "invented"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidatePolicy := policy
			candidateRequest := request
			candidateRuntime := runtime
			test.mutate(&candidatePolicy, &candidateRequest, &candidateRuntime)
			if _, err := Route(candidatePolicy, candidateRequest, candidateRuntime); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Route() error = %v, want InvalidInput", err)
			}
		})
	}
}

func testPolicy() Policy {
	return Policy{
		Version:  "language-routing.v1",
		Template: TemplatePolicy{Enabled: true},
		Local: BackendPolicy{
			Enabled: true, MinimumRemainingTime: 1500 * time.Millisecond,
			MaxInputTokens: 1024, MaxOutputTokens: 256,
		},
		Cloud: CloudPolicy{
			BackendPolicy: BackendPolicy{
				Enabled: true, MinimumRemainingTime: 5 * time.Second,
				MaxInputTokens: 4096, MaxOutputTokens: 512,
			},
			SessionTokenBudget: 8192,
			DailyTokenBudget:   32768,
		},
	}
}

func testRequest(task TaskKind, privacy PrivacyClass, priority Priority) Request {
	return Request{
		ID: "language-request-1", Task: task, Privacy: privacy, Priority: priority,
		Remaining: 6 * time.Second, InputTokens: 128, OutputTokens: 64,
	}
}

func testRuntime() RuntimeSnapshot {
	return RuntimeSnapshot{
		Local:                       BackendRuntime{Health: BackendHealthy, Circuit: CircuitClosed, CapacityAvailable: true},
		Cloud:                       BackendRuntime{Health: BackendHealthy, Circuit: CircuitClosed, CapacityAvailable: true},
		LocalGPUAvailable:           true,
		NetworkAvailable:            true,
		CloudSessionTokensRemaining: 8192,
		CloudDailyTokensRemaining:   32768,
	}
}
