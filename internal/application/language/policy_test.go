package language

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestPolicyValidationRejectsUnsafeOrUnboundedConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Policy)
	}{
		{name: "missing version", mutate: func(p *Policy) { p.Version = "" }},
		{name: "invalid version", mutate: func(p *Policy) { p.Version = "language routing v1" }},
		{name: "template disabled", mutate: func(p *Policy) { p.Template.Enabled = false }},
		{name: "local negative delay", mutate: func(p *Policy) { p.Local.MinimumRemainingTime = -time.Nanosecond }},
		{name: "local zero input", mutate: func(p *Policy) { p.Local.MaxInputTokens = 0 }},
		{name: "local excessive output", mutate: func(p *Policy) { p.Local.MaxOutputTokens = 32769 }},
		{name: "cloud zero session budget", mutate: func(p *Policy) { p.Cloud.SessionTokenBudget = 0 }},
		{name: "cloud daily below session", mutate: func(p *Policy) { p.Cloud.DailyTokenBudget = p.Cloud.SessionTokenBudget - 1 }},
		{name: "cloud excessive daily budget", mutate: func(p *Policy) { p.Cloud.DailyTokenBudget = 100_000_001 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy()
			test.mutate(&policy)
			if err := policy.Validate(); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Validate() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestPolicyValidationAcceptsDocumentedBounds(t *testing.T) {
	policy := testPolicy()
	policy.Local.MinimumRemainingTime = 5 * time.Minute
	policy.Cloud.MinimumRemainingTime = 5 * time.Minute
	policy.Local.MaxInputTokens = 131072
	policy.Local.MaxOutputTokens = 32768
	policy.Cloud.DailyTokenBudget = 100_000_000
	if err := policy.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
