package main

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

// Config contains only explicit local composition values.
type Config struct {
	SubjectID              string
	ListenAddress          string
	PrivacyFile            string
	WASMFile               string
	WASMExecFile           string
	TTSBinary              string
	ReturnAbsenceThreshold time.Duration
	RejectionCooldown      time.Duration
	NoResponseCooldown     time.Duration
	ActionTimeout          time.Duration
	ExternalCallTimeout    time.Duration
	ShutdownTimeout        time.Duration
}

func (c Config) Validate() error {
	const op = "validate desktop configuration"
	if strings.TrimSpace(c.SubjectID) == "" {
		return fault.New(fault.InvalidInput, op, errors.New("subject id is required"))
	}
	host, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fault.New(fault.InvalidInput, op, errors.New("listen address must include an IP and port"))
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fault.New(fault.InvalidInput, op, errors.New("listen address must use an explicit loopback IP"))
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 0 || parsedPort > 65535 {
		return fault.New(fault.InvalidInput, op, errors.New("listen port is invalid"))
	}
	for _, item := range []struct {
		label string
		path  string
	}{
		{label: "privacy file", path: c.PrivacyFile},
		{label: "WASM file", path: c.WASMFile},
		{label: "wasm_exec file", path: c.WASMExecFile},
	} {
		if strings.TrimSpace(item.path) == "" || !filepath.IsAbs(item.path) {
			return fault.New(fault.InvalidInput, op, fmt.Errorf("%s must be an absolute path", item.label))
		}
	}
	if c.TTSBinary != "" && !filepath.IsAbs(c.TTSBinary) {
		return fault.New(fault.InvalidInput, op, errors.New("TTS binary must be an absolute path"))
	}
	if c.ReturnAbsenceThreshold <= 0 || c.RejectionCooldown <= 0 || c.NoResponseCooldown <= 0 ||
		c.ActionTimeout <= 0 || c.ExternalCallTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return fault.New(fault.InvalidInput, op, errors.New("desktop durations must be positive"))
	}
	return nil
}
