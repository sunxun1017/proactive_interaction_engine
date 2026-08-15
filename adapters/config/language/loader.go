package languageconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"proactive-interaction-engine/internal/application/language"
	"proactive-interaction-engine/internal/domain/fault"

	"go.yaml.in/yaml/v3"
)

const (
	loadOp                 = "load language routing policy"
	supportedSchemaVersion = "v1"
)

// LoadedPolicy is one validated v1 declaration and its canonical content hash.
type LoadedPolicy struct {
	SchemaVersion string
	Policy        language.Policy
	Hash          string
}

type strictString struct {
	Present bool
	Value   string
}

func (value *strictString) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("value must be a YAML string")
	}
	value.Present = true
	value.Value = node.Value
	return nil
}

type strictBool struct {
	Present bool
	Value   bool
}

func (value *strictBool) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!bool" || (node.Value != "true" && node.Value != "false") {
		return errors.New("value must be a lowercase YAML boolean")
	}
	value.Present = true
	value.Value = node.Value == "true"
	return nil
}

type strictUint struct {
	Present bool
	Value   uint64
}

func (value *strictUint) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!int" || node.Value == "" {
		return errors.New("value must be a YAML unsigned decimal integer")
	}
	for _, character := range node.Value {
		if character < '0' || character > '9' {
			return errors.New("value must be a YAML unsigned decimal integer")
		}
	}
	parsed, err := strconv.ParseUint(node.Value, 10, 64)
	if err != nil {
		return errors.New("value must fit an unsigned 64-bit integer")
	}
	value.Present = true
	value.Value = parsed
	return nil
}

type wireManifest struct {
	SchemaVersion strictString  `yaml:"schema_version"`
	PolicyVersion strictString  `yaml:"policy_version"`
	Backends      *wireBackends `yaml:"backends"`
}

type wireBackends struct {
	Template *wireTemplate `yaml:"template"`
	Local    *wireBackend  `yaml:"local"`
	Cloud    *wireCloud    `yaml:"cloud"`
}

type wireTemplate struct {
	Enabled strictBool `yaml:"enabled"`
}

type wireBackend struct {
	Enabled              strictBool   `yaml:"enabled"`
	MinimumRemainingTime strictString `yaml:"minimum_remaining_time"`
	MaxInputTokens       strictUint   `yaml:"max_input_tokens"`
	MaxOutputTokens      strictUint   `yaml:"max_output_tokens"`
}

type wireCloud struct {
	Enabled              strictBool   `yaml:"enabled"`
	MinimumRemainingTime strictString `yaml:"minimum_remaining_time"`
	MaxInputTokens       strictUint   `yaml:"max_input_tokens"`
	MaxOutputTokens      strictUint   `yaml:"max_output_tokens"`
	SessionTokenBudget   strictUint   `yaml:"session_token_budget"`
	DailyTokenBudget     strictUint   `yaml:"daily_token_budget"`
}

type canonicalPolicy struct {
	SchemaVersion string            `json:"schema_version"`
	PolicyVersion string            `json:"policy_version"`
	Template      canonicalTemplate `json:"template"`
	Local         canonicalBackend  `json:"local"`
	Cloud         canonicalCloud    `json:"cloud"`
}

type canonicalTemplate struct {
	Enabled bool `json:"enabled"`
}

type canonicalBackend struct {
	Enabled                         bool   `json:"enabled"`
	MinimumRemainingTimeNanoseconds int64  `json:"minimum_remaining_time_nanoseconds"`
	MaxInputTokens                  uint32 `json:"max_input_tokens"`
	MaxOutputTokens                 uint32 `json:"max_output_tokens"`
}

type canonicalCloud struct {
	canonicalBackend
	SessionTokenBudget uint64 `json:"session_token_budget"`
	DailyTokenBudget   uint64 `json:"daily_token_budget"`
}

// Load decodes exactly one strict language-routing v1 YAML document.
func Load(reader io.Reader) (LoadedPolicy, error) {
	if reader == nil {
		return LoadedPolicy{}, invalidInput(errors.New("reader is required"))
	}
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var wire wireManifest
	if err := decoder.Decode(&wire); err != nil {
		return LoadedPolicy{}, invalidInput(fmt.Errorf("decode policy: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return LoadedPolicy{}, invalidInput(fmt.Errorf("decode trailing document: %w", err))
		}
		return LoadedPolicy{}, invalidInput(errors.New("multiple YAML documents are not allowed"))
	}
	if !wire.SchemaVersion.Present || wire.SchemaVersion.Value != supportedSchemaVersion {
		return LoadedPolicy{}, invalidInput(fmt.Errorf("unsupported schema version %q", wire.SchemaVersion.Value))
	}
	if !wire.PolicyVersion.Present || wire.PolicyVersion.Value == "" {
		return LoadedPolicy{}, invalidInput(errors.New("policy_version is required"))
	}
	if wire.Backends == nil || wire.Backends.Template == nil || wire.Backends.Local == nil || wire.Backends.Cloud == nil {
		return LoadedPolicy{}, invalidInput(errors.New("template, local, and cloud backends are required"))
	}

	if !wire.Backends.Template.Enabled.Present {
		return LoadedPolicy{}, invalidInput(errors.New("template backend requires enabled"))
	}
	local, err := mapBackend("local", *wire.Backends.Local)
	if err != nil {
		return LoadedPolicy{}, err
	}
	cloudBackend, err := mapBackend("cloud", wireBackend{
		Enabled:              wire.Backends.Cloud.Enabled,
		MinimumRemainingTime: wire.Backends.Cloud.MinimumRemainingTime,
		MaxInputTokens:       wire.Backends.Cloud.MaxInputTokens,
		MaxOutputTokens:      wire.Backends.Cloud.MaxOutputTokens,
	})
	if err != nil {
		return LoadedPolicy{}, err
	}
	if !wire.Backends.Cloud.SessionTokenBudget.Present || !wire.Backends.Cloud.DailyTokenBudget.Present {
		return LoadedPolicy{}, invalidInput(errors.New("cloud session and daily token budgets are required"))
	}
	policy := language.Policy{
		Version:  wire.PolicyVersion.Value,
		Template: language.TemplatePolicy{Enabled: wire.Backends.Template.Enabled.Value},
		Local:    local,
		Cloud: language.CloudPolicy{
			BackendPolicy:      cloudBackend,
			SessionTokenBudget: wire.Backends.Cloud.SessionTokenBudget.Value,
			DailyTokenBudget:   wire.Backends.Cloud.DailyTokenBudget.Value,
		},
	}
	if err := policy.Validate(); err != nil {
		return LoadedPolicy{}, invalidInput(err)
	}
	hash, err := canonicalHash(wire.SchemaVersion.Value, policy)
	if err != nil {
		return LoadedPolicy{}, invalidInput(err)
	}
	return LoadedPolicy{SchemaVersion: wire.SchemaVersion.Value, Policy: policy, Hash: hash}, nil
}

func mapBackend(name string, wire wireBackend) (language.BackendPolicy, error) {
	if !wire.Enabled.Present || !wire.MinimumRemainingTime.Present || !wire.MaxInputTokens.Present || !wire.MaxOutputTokens.Present {
		return language.BackendPolicy{}, invalidInput(fmt.Errorf("%s backend requires enabled, minimum_remaining_time, max_input_tokens, and max_output_tokens", name))
	}
	minimum, err := time.ParseDuration(wire.MinimumRemainingTime.Value)
	if err != nil {
		return language.BackendPolicy{}, invalidInput(fmt.Errorf("parse %s minimum remaining time: %w", name, err))
	}
	if wire.MaxInputTokens.Value > math.MaxUint32 || wire.MaxOutputTokens.Value > math.MaxUint32 {
		return language.BackendPolicy{}, invalidInput(fmt.Errorf("%s token limit exceeds uint32", name))
	}
	return language.BackendPolicy{
		Enabled:              wire.Enabled.Value,
		MinimumRemainingTime: minimum,
		MaxInputTokens:       uint32(wire.MaxInputTokens.Value),
		MaxOutputTokens:      uint32(wire.MaxOutputTokens.Value),
	}, nil
}

func canonicalHash(schemaVersion string, policy language.Policy) (string, error) {
	canonical := canonicalPolicy{
		SchemaVersion: schemaVersion,
		PolicyVersion: policy.Version,
		Template:      canonicalTemplate{Enabled: policy.Template.Enabled},
		Local:         canonicalizeBackend(policy.Local),
		Cloud: canonicalCloud{
			canonicalBackend:   canonicalizeBackend(policy.Cloud.BackendPolicy),
			SessionTokenBudget: policy.Cloud.SessionTokenBudget,
			DailyTokenBudget:   policy.Cloud.DailyTokenBudget,
		},
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical policy: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalizeBackend(policy language.BackendPolicy) canonicalBackend {
	return canonicalBackend{
		Enabled:                         policy.Enabled,
		MinimumRemainingTimeNanoseconds: policy.MinimumRemainingTime.Nanoseconds(),
		MaxInputTokens:                  policy.MaxInputTokens,
		MaxOutputTokens:                 policy.MaxOutputTokens,
	}
}

func invalidInput(err error) error {
	return fault.New(fault.InvalidInput, loadOp, err)
}
