package languageconfig

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/language"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestLoadStrictV1Policy(t *testing.T) {
	loaded := mustLoad(t, validManifest())
	want := language.Policy{
		Version:  "language-routing.v1",
		Template: language.TemplatePolicy{Enabled: true},
		Local: language.BackendPolicy{
			Enabled: true, MinimumRemainingTime: 1500 * time.Millisecond,
			MaxInputTokens: 1024, MaxOutputTokens: 256,
		},
		Cloud: language.CloudPolicy{
			BackendPolicy: language.BackendPolicy{
				Enabled: false, MinimumRemainingTime: 5 * time.Second,
				MaxInputTokens: 4096, MaxOutputTokens: 512,
			},
			SessionTokenBudget: 8192,
			DailyTokenBudget:   32768,
		},
	}
	if !reflect.DeepEqual(loaded.Policy, want) {
		t.Fatalf("Load() policy = %#v, want %#v", loaded.Policy, want)
	}
	if loaded.SchemaVersion != "v1" {
		t.Fatalf("SchemaVersion = %q, want v1", loaded.SchemaVersion)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(loaded.Hash) {
		t.Fatalf("Hash = %q, want lowercase SHA-256", loaded.Hash)
	}
}

func TestLoadRejectsUnknownMissingOrMalformedFields(t *testing.T) {
	valid := validManifest()
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "empty"},
		{name: "malformed", manifest: "schema_version: ["},
		{name: "bad schema", manifest: strings.Replace(valid, "schema_version: v1", "schema_version: v2", 1)},
		{name: "numeric schema", manifest: strings.Replace(valid, "schema_version: v1", "schema_version: 1", 1)},
		{name: "missing schema", manifest: strings.Replace(valid, "schema_version: v1\n", "", 1)},
		{name: "missing policy version", manifest: strings.Replace(valid, "policy_version: language-routing.v1\n", "", 1)},
		{name: "numeric policy version", manifest: strings.Replace(valid, "policy_version: language-routing.v1", "policy_version: 1", 1)},
		{name: "missing backends", manifest: "schema_version: v1\npolicy_version: language-routing.v1\n"},
		{name: "missing template", manifest: strings.Replace(valid, templateBlock(), "", 1)},
		{name: "missing local", manifest: strings.Replace(valid, localBlock(), "", 1)},
		{name: "missing cloud", manifest: strings.Replace(valid, cloudBlock(), "", 1)},
		{name: "missing enabled", manifest: strings.Replace(valid, "      enabled: true\n", "", 1)},
		{name: "string enabled", manifest: strings.Replace(valid, "enabled: true", "enabled: 'true'", 1)},
		{name: "numeric duration", manifest: strings.Replace(valid, "minimum_remaining_time: 1500ms", "minimum_remaining_time: 1500", 1)},
		{name: "missing duration", manifest: strings.Replace(valid, "      minimum_remaining_time: 1500ms\n", "", 1)},
		{name: "invalid duration", manifest: strings.Replace(valid, "1500ms", "soon", 1)},
		{name: "negative duration", manifest: strings.Replace(valid, "1500ms", "-1ms", 1)},
		{name: "zero local duration", manifest: strings.Replace(valid, "1500ms", "0s", 1)},
		{name: "too long cloud duration", manifest: strings.Replace(valid, "5s", "5m1ns", 1)},
		{name: "missing input limit", manifest: strings.Replace(valid, "      max_input_tokens: 1024\n", "", 1)},
		{name: "string input limit", manifest: strings.Replace(valid, "max_input_tokens: 1024", "max_input_tokens: '1024'", 1)},
		{name: "zero output limit", manifest: strings.Replace(valid, "max_output_tokens: 256", "max_output_tokens: 0", 1)},
		{name: "overflow uint32", manifest: strings.Replace(valid, "max_input_tokens: 4096", "max_input_tokens: 4294967296", 1)},
		{name: "missing session budget", manifest: strings.Replace(valid, "      session_token_budget: 8192\n", "", 1)},
		{name: "zero session budget", manifest: strings.Replace(valid, "session_token_budget: 8192", "session_token_budget: 0", 1)},
		{name: "daily below session", manifest: strings.Replace(valid, "daily_token_budget: 32768", "daily_token_budget: 8191", 1)},
		{name: "unknown root", manifest: valid + "unknown: true\n"},
		{name: "unknown backend", manifest: strings.Replace(valid, "  template:\n", "  template:\n      unknown: value\n", 1)},
		{name: "duplicate key", manifest: strings.Replace(valid, "policy_version: language-routing.v1\n", "policy_version: language-routing.v1\npolicy_version: duplicate\n", 1)},
		{name: "multiple documents", manifest: valid + "---\n" + valid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(strings.NewReader(test.manifest)); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Load() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestLoadAcceptsDocumentedBoundaryValues(t *testing.T) {
	manifest := validManifest()
	manifest = strings.Replace(manifest, "1500ms", "1ns", 1)
	manifest = strings.Replace(manifest, "5s", "5m", 1)
	manifest = strings.Replace(manifest, "max_input_tokens: 1024", "max_input_tokens: 131072", 1)
	manifest = strings.Replace(manifest, "max_output_tokens: 256", "max_output_tokens: 32768", 1)
	manifest = strings.Replace(manifest, "daily_token_budget: 32768", "daily_token_budget: 100000000", 1)
	loaded, err := Load(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Load(boundaries) error = %v", err)
	}
	if loaded.Policy.Local.MinimumRemainingTime != time.Nanosecond || loaded.Policy.Cloud.MinimumRemainingTime != 5*time.Minute ||
		loaded.Policy.Local.MaxInputTokens != 131072 || loaded.Policy.Local.MaxOutputTokens != 32768 || loaded.Policy.Cloud.DailyTokenBudget != 100_000_000 {
		t.Fatalf("Load(boundaries) policy = %#v", loaded.Policy)
	}
}

func TestLoadCanonicalHashIncludesEverySemanticField(t *testing.T) {
	baselineManifest := validManifest()
	baseline := mustLoad(t, baselineManifest)
	variants := []string{
		strings.Replace(baselineManifest, "language-routing.v1", "language-routing.v2", 1),
		strings.Replace(baselineManifest, "  local:\n      enabled: true", "  local:\n      enabled: false", 1),
		strings.Replace(baselineManifest, "1500ms", "1501ms", 1),
		strings.Replace(baselineManifest, "max_input_tokens: 1024", "max_input_tokens: 1025", 1),
		strings.Replace(baselineManifest, "max_output_tokens: 256", "max_output_tokens: 257", 1),
		strings.Replace(baselineManifest, "enabled: false", "enabled: true", 1),
		strings.Replace(baselineManifest, "5s", "5001ms", 1),
		strings.Replace(baselineManifest, "max_input_tokens: 4096", "max_input_tokens: 4097", 1),
		strings.Replace(baselineManifest, "max_output_tokens: 512", "max_output_tokens: 513", 1),
		strings.Replace(baselineManifest, "session_token_budget: 8192", "session_token_budget: 8193", 1),
		strings.Replace(baselineManifest, "daily_token_budget: 32768", "daily_token_budget: 32769", 1),
	}
	for index, manifest := range variants {
		loaded, err := Load(strings.NewReader(manifest))
		if err != nil {
			t.Fatalf("Load(variant %d) error = %v", index, err)
		}
		if loaded.Hash == baseline.Hash {
			t.Fatalf("variant %d hash = baseline %q", index, baseline.Hash)
		}
	}

	equivalent := strings.Replace(baselineManifest, "1500ms", "1.5s", 1)
	if got := mustLoad(t, equivalent).Hash; got != baseline.Hash {
		t.Fatalf("equivalent duration hash = %q, want %q", got, baseline.Hash)
	}
}

func TestLoadReaderFailureIsInvalidInput(t *testing.T) {
	if _, err := Load(errorReader{err: errors.New("read failed")}); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Load(error reader) error = %v, want InvalidInput", err)
	}
}

func TestCheckedInV1PolicyLoads(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test path")
	}
	path := filepath.Join(filepath.Dir(source), "..", "..", "..", "configs", "language-routing.v1.yaml")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open checked-in policy: %v", err)
	}
	defer file.Close()
	loaded, err := Load(file)
	if err != nil {
		t.Fatalf("Load(checked-in policy) error = %v", err)
	}
	if loaded.Policy.Version != "language-routing.v1" || loaded.Policy.Local.Enabled || loaded.Policy.Cloud.Enabled {
		t.Fatalf("checked-in policy = %#v, want template-only v1", loaded.Policy)
	}
}

func validManifest() string {
	return "schema_version: v1\npolicy_version: language-routing.v1\nbackends:\n" + templateBlock() + localBlock() + cloudBlock()
}

func templateBlock() string {
	return `  template:
      enabled: true
`
}

func localBlock() string {
	return `  local:
      enabled: true
      minimum_remaining_time: 1500ms
      max_input_tokens: 1024
      max_output_tokens: 256
`
}

func cloudBlock() string {
	return `  cloud:
      enabled: false
      minimum_remaining_time: 5s
      max_input_tokens: 4096
      max_output_tokens: 512
      session_token_budget: 8192
      daily_token_budget: 32768
`
}

func mustLoad(t *testing.T, manifest string) LoadedPolicy {
	t.Helper()
	loaded, err := Load(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return loaded
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = errorReader{}
