package identitycalibration

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestLoadStrictSyntheticV1Artifact(t *testing.T) {
	loaded := mustLoadGolden(t)
	if loaded.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", loaded.SchemaVersion)
	}
	if loaded.Artifact.CalibrationID != "synthetic-identity-calibration.v1" || loaded.Artifact.PolicyVersion != "synthetic-identity-policy.v1" {
		t.Fatalf("artifact identity = %#v", loaded.Artifact)
	}
	if len(loaded.Artifact.Models) != 4 || loaded.Artifact.Models[1].Role != ModelRoleFaceEmbedding {
		t.Fatalf("models = %#v", loaded.Artifact.Models)
	}
	if loaded.Artifact.Protocols.FaceIdentification.ScoreMapping != ScoreMappingAffineCosineV1 {
		t.Fatalf("face score mapping = %q", loaded.Artifact.Protocols.FaceIdentification.ScoreMapping)
	}
	if len(loaded.Artifact.ProviderLatency) != 5 || loaded.Artifact.ProviderLatency[4].Capability != CapabilitySpeakerVerification {
		t.Fatalf("provider latency = %#v", loaded.Artifact.ProviderLatency)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(loaded.Hash) {
		t.Fatalf("Hash = %q, want lowercase SHA-256", loaded.Hash)
	}
	wantHash := strings.TrimSpace(string(readTestdata(t, "synthetic-valid.v1.sha256")))
	if loaded.Hash != wantHash {
		t.Fatalf("Hash = %q, want shared golden %q", loaded.Hash, wantHash)
	}
}

func TestLoadRejectsNonStrictJSON(t *testing.T) {
	valid := string(readTestdata(t, "synthetic-valid.v1.json"))
	tests := []struct {
		name     string
		artifact string
	}{
		{name: "empty"},
		{name: "malformed", artifact: "{"},
		{name: "duplicate", artifact: strings.Replace(valid, `"schema_version": 1,`, `"schema_version": 1, "schema_version": 1,`, 1)},
		{name: "unknown", artifact: strings.Replace(valid, `"calibration_id":`, `"unknown": true, "calibration_id":`, 1)},
		{name: "nested unknown", artifact: strings.Replace(valid, `"inference_mode":`, `"unknown": true, "inference_mode":`, 1)},
		{name: "null", artifact: strings.Replace(valid, `"calibration_id": "synthetic-identity-calibration.v1"`, `"calibration_id": null`, 1)},
		{name: "coerced schema", artifact: strings.Replace(valid, `"schema_version": 1`, `"schema_version": "1"`, 1)},
		{name: "float", artifact: strings.Replace(valid, `"score_threshold_ppm": 650000`, `"score_threshold_ppm": 650000.0`, 1)},
		{name: "exponent", artifact: strings.Replace(valid, `"sample_count": 1000`, `"sample_count": 1e3`, 1)},
		{name: "negative", artifact: strings.Replace(valid, `"cuda_device_index": 0`, `"cuda_device_index": -1`, 1)},
		{name: "trailing value", artifact: valid + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(strings.NewReader(test.artifact)); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Load() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestLoadRejectsUnsafeOrIncompleteArtifact(t *testing.T) {
	valid := string(readTestdata(t, "synthetic-valid.v1.json"))
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "bad schema", old: `"schema_version": 1`, new: `"schema_version": 2`},
		{name: "missing policy", old: "  \"policy_version\": \"synthetic-identity-policy.v1\",\n", new: ""},
		{name: "missing zero-valued cuda index", old: "    \"cuda_device_index\": 0,\n", new: ""},
		{name: "authentication scope", old: `"usage_scope": "PERSONALIZATION_ONLY"`, new: `"usage_scope": "AUTHENTICATION"`},
		{name: "security enabled", old: `"security_authentication_allowed": false`, new: `"security_authentication_allowed": true`},
		{name: "duplicate model role", old: `"role": "FACE_EMBEDDING"`, new: `"role": "FACE_DETECTION"`},
		{name: "bad model digest", old: `"sha256": "8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4"`, new: `"sha256": "bad"`},
		{name: "cuda not required", old: `"cuda_required": true`, new: `"cuda_required": false`},
		{name: "cpu fallback", old: `"cpu_fallback_allowed": false`, new: `"cpu_fallback_allowed": true`},
		{name: "wrong inference mode", old: `"inference_mode": "CUDA_ONLY"`, new: `"inference_mode": "CPU"`},
		{name: "raw face cosine", old: `"score_mapping": "AFFINE_COSINE_V1"`, new: `"score_mapping": "RAW_COSINE_V1"`},
		{name: "zero liveness threshold", old: `"threshold_ppm": 700000`, new: `"threshold_ppm": 0`},
		{name: "liveness threshold overflow", old: `"threshold_ppm": 700000`, new: `"threshold_ppm": 1000001`},
		{name: "speaker clip inverted", old: `"maximum_clip_us": 5000000`, new: `"maximum_clip_us": 500000`},
		{name: "speaker enrollment inverted", old: `"enrollment_maximum_clips": 8`, new: `"enrollment_maximum_clips": 2`},
		{name: "application threshold zero", old: `"face_identification_threshold_ppm": 800000`, new: `"face_identification_threshold_ppm": 0`},
		{name: "liveness optional", old: `"require_face_liveness": true`, new: `"require_face_liveness": false`},
		{name: "duplicate provider capability", old: `"capability": "FACE_IDENTIFICATION"`, new: `"capability": "FACE_DETECTION"`},
		{name: "zero latency samples", old: `"sample_count": 1000`, new: `"sample_count": 0`},
		{name: "latency order", old: `"p95_us": 40000`, new: `"p95_us": 70000`},
		{name: "bad provenance", old: `"aggregate_curve_report_sha256": "1111111111111111111111111111111111111111111111111111111111111111"`, new: `"aggregate_curve_report_sha256": "bad"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := strings.Replace(valid, test.old, test.new, 1)
			if artifact == valid {
				t.Fatalf("fixture does not contain %q", test.old)
			}
			if _, err := Load(strings.NewReader(artifact)); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Load() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestLoadCanonicalHashIgnoresJSONFormattingAndPropertyOrder(t *testing.T) {
	valid := readTestdata(t, "synthetic-valid.v1.json")
	baseline := mustLoad(t, valid)
	compact := bytes.ReplaceAll(valid, []byte("\n"), nil)
	compact = bytes.Replace(compact,
		[]byte(`"calibration_id": "synthetic-identity-calibration.v1",  "policy_version": "synthetic-identity-policy.v1"`),
		[]byte(`"policy_version": "synthetic-identity-policy.v1",  "calibration_id": "synthetic-identity-calibration.v1"`), 1)
	loaded := mustLoad(t, compact)
	if loaded.Hash != baseline.Hash {
		t.Fatalf("reformatted hash = %q, want %q", loaded.Hash, baseline.Hash)
	}
}

func TestLoadReaderFailuresAreInvalidInput(t *testing.T) {
	if _, err := Load(nil); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Load(nil) error = %v, want InvalidInput", err)
	}
	if _, err := Load(errorReader{err: errors.New("read failed")}); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Load(error reader) error = %v, want InvalidInput", err)
	}
}

func mustLoadGolden(t *testing.T) LoadedArtifact {
	t.Helper()
	return mustLoad(t, readTestdata(t, "synthetic-valid.v1.json"))
}

func mustLoad(t *testing.T, content []byte) LoadedArtifact {
	t.Helper()
	loaded, err := Load(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return loaded
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return content
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

var _ io.Reader = errorReader{}
