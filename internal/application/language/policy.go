package language

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

const (
	validatePolicyOp = "validate language routing policy"
	maxBackendDelay  = 5 * time.Minute
	maxInputTokens   = 131072
	maxOutputTokens  = 32768
	maxCloudBudget   = 100_000_000
)

var policyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (p Policy) Validate() error {
	if !policyVersionPattern.MatchString(p.Version) {
		return invalidPolicy(errors.New("policy version must be a bounded identifier"))
	}
	if !p.Template.Enabled {
		return invalidPolicy(errors.New("template backend must be enabled"))
	}
	if err := validateBackendPolicy("local", p.Local); err != nil {
		return err
	}
	if err := validateBackendPolicy("cloud", p.Cloud.BackendPolicy); err != nil {
		return err
	}
	if p.Cloud.SessionTokenBudget == 0 || p.Cloud.SessionTokenBudget > maxCloudBudget {
		return invalidPolicy(fmt.Errorf("cloud session token budget must be in [1,%d]", maxCloudBudget))
	}
	if p.Cloud.DailyTokenBudget < p.Cloud.SessionTokenBudget || p.Cloud.DailyTokenBudget > maxCloudBudget {
		return invalidPolicy(fmt.Errorf("cloud daily token budget must be in [session,%d]", maxCloudBudget))
	}
	return nil
}

func validateBackendPolicy(name string, policy BackendPolicy) error {
	if policy.MinimumRemainingTime < 0 || policy.MinimumRemainingTime > maxBackendDelay {
		return invalidPolicy(fmt.Errorf("%s minimum remaining time must be in (0,%s]", name, maxBackendDelay))
	}
	if policy.MinimumRemainingTime == 0 {
		return invalidPolicy(fmt.Errorf("%s minimum remaining time must be positive", name))
	}
	if policy.MaxInputTokens == 0 || policy.MaxInputTokens > maxInputTokens {
		return invalidPolicy(fmt.Errorf("%s max input tokens must be in [1,%d]", name, maxInputTokens))
	}
	if policy.MaxOutputTokens == 0 || policy.MaxOutputTokens > maxOutputTokens {
		return invalidPolicy(fmt.Errorf("%s max output tokens must be in [1,%d]", name, maxOutputTokens))
	}
	return nil
}

func invalidPolicy(err error) error {
	return fault.New(fault.InvalidInput, validatePolicyOp, err)
}
