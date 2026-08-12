package behavior

import (
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/fault"
)

type NodeKind string

const (
	Sequence  NodeKind = "SEQUENCE"
	Parallel  NodeKind = "PARALLEL"
	Race      NodeKind = "RACE"
	Action    NodeKind = "ACTION"
	WaitEvent NodeKind = "WAIT_EVENT"
	Condition NodeKind = "CONDITION"
)

type ActionType string

const (
	AttendUser  ActionType = "ATTEND_USER"
	Acknowledge ActionType = "ACKNOWLEDGE"
	Speak       ActionType = "SPEAK"
	Express     ActionType = "EXPRESS"
	ReturnIdle  ActionType = "RETURN_IDLE"
)

type Resource string

const (
	Voice      Resource = "VOICE"
	Attention  Resource = "ATTENTION"
	Display    Resource = "DISPLAY"
	Gesture    Resource = "GESTURE"
	Locomotion Resource = "LOCOMOTION"
)

type Capability struct {
	Supported     bool
	Interruptible bool
}

type Capabilities map[ActionType]Capability

type Node struct {
	Kind      NodeKind
	Children  []Node
	Action    *ActionSpec
	WaitFor   time.Duration
	EventType string
	Condition string
}

type ActionSpec struct {
	Type       ActionType
	TemplateID string
	Resource   Resource
	Timeout    time.Duration
}

type BehaviorPlan struct {
	ID              string
	InteractionID   string
	BehaviorVersion string
	TraceID         string
	Root            Node
}

type PreemptionPolicy string

const (
	PreemptNever         PreemptionPolicy = "NEVER"
	PreemptInterruptible PreemptionPolicy = "INTERRUPTIBLE"
)

type Idempotency string

const IdempotentByActionID Idempotency = "ACTION_ID"

type ActionCommand struct {
	ID                 string
	InteractionID      string
	TraceID            string
	Deadline           time.Time
	Preemption         PreemptionPolicy
	RequiredCapability ActionType
	Idempotency        Idempotency
	Action             ActionSpec
}

type phaseAction struct {
	ordinal int
	action  ActionSpec
}

// ActionPhase is one side of the single supported user-reply wait. It retains
// original plan ordinals while materializing deadlines only when dispatched.
type ActionPhase struct {
	interactionID string
	traceID       string
	actions       []phaseAction
}

// UserReplyPhases is a narrow compilation of the current root Sequence around
// its one user.reply wait. It is not a general behavior cursor.
type UserReplyPhases struct {
	Before  ActionPhase
	After   ActionPhase
	WaitFor time.Duration
}

type ActionState string

const (
	ActionCreated    ActionState = "CREATED"
	ActionDispatched ActionState = "DISPATCHED"
	ActionAccepted   ActionState = "ACCEPTED"
	ActionStarted    ActionState = "STARTED"
	ActionCompleted  ActionState = "COMPLETED"
	ActionRejected   ActionState = "REJECTED"
	ActionCancelled  ActionState = "CANCELLED"
	ActionTimedOut   ActionState = "TIMED_OUT"
	ActionFailed     ActionState = "FAILED"
)

type ActionStatus struct {
	ActionID      string
	InteractionID string
	TraceID       string
	State         ActionState
	OccurredAt    time.Time
	Reason        string
}

type Planner struct {
	ActionTimeout time.Duration
}

// Plan compiles a greeting decision and omits every unsupported optional action.
func (p Planner) Plan(input decision.Decision, capabilities Capabilities, now time.Time) (BehaviorPlan, error) {
	if input.Kind == decision.Silent {
		return BehaviorPlan{}, fault.New(fault.PolicyBlocked, "compile behavior plan", errors.New("silent decision has no behavior plan"))
	}
	if input.Kind != decision.GreetShort {
		return BehaviorPlan{}, fault.New(fault.InvalidInput, "compile behavior plan", fmt.Errorf("unsupported decision %s", input.Kind))
	}

	candidates := []Node{
		actionNode(AttendUser, "", Attention, 200*time.Millisecond),
		{Kind: Condition, Condition: "attend_user_completed_or_skipped"},
		actionNode(Acknowledge, "", Gesture, 500*time.Millisecond),
		actionNode(Speak, "welcome.return", Voice, p.ActionTimeout),
		{Kind: WaitEvent, EventType: "user.reply", WaitFor: 8 * time.Second},
		actionNode(ReturnIdle, "", Attention, p.ActionTimeout),
	}

	children := make([]Node, 0, len(candidates))
	hasFeedback := false
	for _, node := range candidates {
		if node.Kind != Action {
			children = append(children, node)
			continue
		}
		capability := capabilities[node.Action.Type]
		if !capability.Supported {
			continue
		}
		children = append(children, node)
		if node.Action.Type == Acknowledge || node.Action.Type == Speak || node.Action.Type == Express {
			hasFeedback = true
		}
	}
	if !hasFeedback {
		return BehaviorPlan{}, fault.New(fault.CapabilityMissing, "compile behavior plan", errors.New("no supported user-feedback capability"))
	}

	plan := BehaviorPlan{
		ID:              "plan:" + input.ID,
		InteractionID:   input.InteractionID,
		BehaviorVersion: input.BehaviorVersion,
		TraceID:         input.TraceID,
		Root:            Node{Kind: Sequence, Children: children},
	}
	if err := plan.ValidateCapabilities(capabilities); err != nil {
		return BehaviorPlan{}, err
	}
	return plan, nil
}

func actionNode(kind ActionType, templateID string, resource Resource, timeout time.Duration) Node {
	return Node{
		Kind: Action,
		Action: &ActionSpec{
			Type:       kind,
			TemplateID: templateID,
			Resource:   resource,
			Timeout:    timeout,
		},
	}
}

func (p BehaviorPlan) ValidateCapabilities(capabilities Capabilities) error {
	for _, action := range p.Actions() {
		if !capabilities[action.Type].Supported {
			return fault.New(fault.CapabilityMissing, "validate behavior plan", fmt.Errorf("action %s is not supported", action.Type))
		}
	}
	return nil
}

// Actions returns action nodes in deterministic traversal order.
func (p BehaviorPlan) Actions() []ActionSpec {
	var output []ActionSpec
	var walk func(Node)
	walk = func(node Node) {
		if node.Kind == Action && node.Action != nil {
			output = append(output, *node.Action)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(p.Root)
	return output
}

// Commands materializes action commands with stable IDs and deadlines.
func (p BehaviorPlan) Commands(now time.Time) []ActionCommand {
	actions := p.Actions()
	phaseActions := make([]phaseAction, 0, len(actions))
	for index, action := range actions {
		phaseActions = append(phaseActions, phaseAction{ordinal: index + 1, action: action})
	}
	return (ActionPhase{interactionID: p.InteractionID, traceID: p.TraceID, actions: phaseActions}).Commands(now)
}

// Commands materializes this phase with deadlines relative to its actual
// dispatch time while retaining global action IDs from the original plan.
func (p ActionPhase) Commands(now time.Time) []ActionCommand {
	commands := make([]ActionCommand, 0, len(p.actions))
	for _, item := range p.actions {
		commands = append(commands, ActionCommand{
			ID:                 fmt.Sprintf("%s:action:%d", p.interactionID, item.ordinal),
			InteractionID:      p.interactionID,
			TraceID:            p.traceID,
			Deadline:           now.Add(item.action.Timeout),
			Preemption:         PreemptInterruptible,
			RequiredCapability: item.action.Type,
			Idempotency:        IdempotentByActionID,
			Action:             item.action,
		})
	}
	return commands
}

// CompileUserReplyPhases validates and splits the currently supported root
// Sequence around exactly one direct user.reply WaitEvent.
func CompileUserReplyPhases(plan BehaviorPlan) (UserReplyPhases, error) {
	const op = "compile user reply phases"
	if plan.Root.Kind != Sequence {
		return UserReplyPhases{}, fault.New(fault.InvalidInput, op, errors.New("behavior root must be SEQUENCE"))
	}
	newPhase := func(actions []phaseAction) ActionPhase {
		return ActionPhase{interactionID: plan.InteractionID, traceID: plan.TraceID, actions: actions}
	}
	var before, after []phaseAction
	waitFound := false
	waitFor := time.Duration(0)
	ordinal := 0
	for _, node := range plan.Root.Children {
		if len(node.Children) != 0 {
			return UserReplyPhases{}, fault.New(fault.InvalidInput, op, errors.New("root sequence children must be direct leaves"))
		}
		switch node.Kind {
		case Action:
			if node.Action == nil {
				return UserReplyPhases{}, fault.New(fault.InvalidInput, op, errors.New("action node requires action spec"))
			}
			ordinal++
			item := phaseAction{ordinal: ordinal, action: *node.Action}
			if waitFound {
				after = append(after, item)
			} else {
				before = append(before, item)
			}
		case WaitEvent:
			if waitFound || node.EventType != "user.reply" || node.WaitFor <= 0 {
				return UserReplyPhases{}, fault.New(fault.InvalidInput, op, errors.New("exactly one positive user.reply wait is required"))
			}
			waitFound = true
			waitFor = node.WaitFor
		case Condition:
		case Sequence, Parallel, Race:
			return UserReplyPhases{}, fault.New(fault.InvalidInput, op, errors.New("nested behavior nodes are not supported by user reply phases"))
		default:
			return UserReplyPhases{}, fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported root child %q", node.Kind))
		}
	}
	if !waitFound {
		return UserReplyPhases{}, fault.New(fault.InvalidInput, op, errors.New("user.reply wait is required"))
	}
	return UserReplyPhases{Before: newPhase(before), After: newPhase(after), WaitFor: waitFor}, nil
}
