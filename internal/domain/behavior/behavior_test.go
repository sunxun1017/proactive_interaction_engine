package behavior

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestPlannerNeverEmitsUnsupportedCapability(t *testing.T) {
	planner := Planner{ActionTimeout: time.Second}
	input := decision.Decision{
		ID:              "decision-1",
		InteractionID:   "interaction-1",
		Kind:            decision.GreetShort,
		BehaviorVersion: "welcome.v1",
		TraceID:         "trace-1",
	}
	capabilities := Capabilities{
		Acknowledge: {Supported: true, Interruptible: true},
		ReturnIdle:  {Supported: true, Interruptible: true},
	}

	plan, err := planner.Plan(input, capabilities, time.Time{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	for _, action := range plan.Actions() {
		if action.Type == AttendUser || action.Type == Speak {
			t.Fatalf("Plan() emitted unsupported action %s", action.Type)
		}
	}
}

func TestCompileUserReplyPhasesPreservesActionOrdinals(t *testing.T) {
	plan := welcomePlan(t)
	phases, err := CompileUserReplyPhases(plan)
	if err != nil {
		t.Fatalf("CompileUserReplyPhases() error = %v", err)
	}
	if phases.WaitFor != 8*time.Second {
		t.Fatalf("WaitFor = %s, want 8s", phases.WaitFor)
	}
	preAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	postAt := preAt.Add(4 * time.Second)
	pre := phases.Before.Commands(preAt)
	post := phases.After.Commands(postAt)
	if got := actionTypes(pre); !equalActionTypes(got, []ActionType{AttendUser, Acknowledge, Speak}) {
		t.Fatalf("pre actions = %#v", got)
	}
	if got := actionTypes(post); !equalActionTypes(got, []ActionType{ReturnIdle}) {
		t.Fatalf("post actions = %#v", got)
	}
	if len(pre) != 3 || pre[0].ID != plan.InteractionID+":action:1" || pre[2].ID != plan.InteractionID+":action:3" {
		t.Fatalf("pre IDs = %#v", pre)
	}
	if len(post) != 1 || post[0].ID != plan.InteractionID+":action:4" {
		t.Fatalf("post IDs = %#v", post)
	}
	if post[0].Deadline != postAt.Add(post[0].Action.Timeout) {
		t.Fatalf("post deadline = %s, want materialized at dispatch", post[0].Deadline)
	}
}

func TestCompileUserReplyPhasesRejectsMissingOrRepeatedWait(t *testing.T) {
	base := welcomePlan(t)
	missing := base
	missing.Root.Children = append([]Node(nil), base.Root.Children...)
	for index, node := range missing.Root.Children {
		if node.Kind == WaitEvent {
			missing.Root.Children = append(missing.Root.Children[:index], missing.Root.Children[index+1:]...)
			break
		}
	}
	repeated := base
	repeated.Root.Children = append(append([]Node(nil), base.Root.Children...), Node{
		Kind: WaitEvent, EventType: "user.reply", WaitFor: time.Second,
	})
	wrongEvent := replaceReplyWait(base, func(node Node) Node {
		node.EventType = "user.other"
		return node
	})
	zeroDuration := replaceReplyWait(base, func(node Node) Node {
		node.WaitFor = 0
		return node
	})
	nonSequence := base
	nonSequence.Root.Kind = Parallel
	nestedWait := base
	nestedWait.Root.Children = append(append([]Node(nil), base.Root.Children...), Node{
		Kind:     Sequence,
		Children: []Node{{Kind: WaitEvent, EventType: "user.reply", WaitFor: time.Second}},
	})
	malformedAction := base
	malformedAction.Root.Children = append([]Node(nil), base.Root.Children...)
	malformedAction.Root.Children[0].Children = []Node{{Kind: WaitEvent, EventType: "user.reply", WaitFor: time.Second}}
	malformedWait := replaceReplyWait(base, func(node Node) Node {
		node.Children = []Node{{Kind: Action, Action: &ActionSpec{Type: ReturnIdle, Timeout: time.Second}}}
		return node
	})

	for name, plan := range map[string]BehaviorPlan{
		"missing":                 missing,
		"repeated":                repeated,
		"wrong event":             wrongEvent,
		"zero duration":           zeroDuration,
		"non sequence":            nonSequence,
		"nested wait":             nestedWait,
		"action with nested wait": malformedAction,
		"wait with child":         malformedWait,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CompileUserReplyPhases(plan); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("CompileUserReplyPhases() error = %v, want InvalidInput", err)
			}
		})
	}
}

func replaceReplyWait(plan BehaviorPlan, replace func(Node) Node) BehaviorPlan {
	plan.Root.Children = append([]Node(nil), plan.Root.Children...)
	for index, node := range plan.Root.Children {
		if node.Kind == WaitEvent {
			plan.Root.Children[index] = replace(node)
			break
		}
	}
	return plan
}

func welcomePlan(t *testing.T) BehaviorPlan {
	t.Helper()
	plan, err := (Planner{ActionTimeout: time.Second}).Plan(decision.Decision{
		ID:              "decision-1",
		InteractionID:   "interaction-1",
		Kind:            decision.GreetShort,
		BehaviorVersion: "welcome.v1",
		TraceID:         "trace-1",
	}, Capabilities{
		AttendUser:  {Supported: true, Interruptible: true},
		Acknowledge: {Supported: true, Interruptible: true},
		Speak:       {Supported: true, Interruptible: true},
		ReturnIdle:  {Supported: true, Interruptible: true},
	}, time.Time{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	return plan
}

func actionTypes(commands []ActionCommand) []ActionType {
	output := make([]ActionType, 0, len(commands))
	for _, command := range commands {
		output = append(output, command.Action.Type)
	}
	return output
}

func equalActionTypes(left, right []ActionType) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
