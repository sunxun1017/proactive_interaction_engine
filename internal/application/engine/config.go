package engine

import (
	"errors"
	"time"
)

type Config struct {
	SubjectID              string
	ReturnAbsenceThreshold time.Duration
	RejectionCooldown      time.Duration
	NoResponseCooldown     time.Duration
	ActionTimeout          time.Duration
	ExternalCallTimeout    time.Duration
	PolicyVersion          string
	BehaviorVersion        string
	ConfigHash             string
	RandomSeed             int64
}

func (c Config) Validate() error {
	if c.SubjectID == "" || c.PolicyVersion == "" || c.BehaviorVersion == "" || c.ConfigHash == "" {
		return errors.New("subject, policy version, behavior version, and config hash are required")
	}
	if c.ReturnAbsenceThreshold <= 0 {
		return errors.New("return absence threshold must be positive")
	}
	if c.RejectionCooldown <= 0 {
		return errors.New("rejection cooldown must be positive")
	}
	if c.NoResponseCooldown <= 0 {
		return errors.New("no-response cooldown must be positive")
	}
	if c.ActionTimeout <= 0 || c.ExternalCallTimeout <= 0 {
		return errors.New("action and external call timeouts must be positive")
	}
	return nil
}
