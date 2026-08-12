package architecture_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDependencyDirection(t *testing.T) {
	root := repositoryRoot(t)
	rules := []struct {
		directory string
		forbidden []string
	}{
		{
			directory: "internal/domain",
			forbidden: []string{
				"proactive-interaction-engine/adapters",
				"proactive-interaction-engine/contracts",
				"proactive-interaction-engine/gen",
				"proactive-interaction-engine/internal/application",
				"proactive-interaction-engine/internal/runtime",
			},
		},
		{
			directory: "internal/application",
			forbidden: []string{
				"proactive-interaction-engine/adapters",
				"proactive-interaction-engine/gen",
				"proactive-interaction-engine/internal/runtime",
				"github.com/ros",
				"modernc.org/sqlite",
			},
		},
	}

	for _, rule := range rules {
		directory := filepath.Join(root, rule.directory)
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			for _, imported := range parsed.Imports {
				value, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Errorf("unquote import in %s: %v", path, err)
					continue
				}
				for _, forbidden := range rule.forbidden {
					if strings.HasPrefix(value, forbidden) {
						position := parsed.Pos()
						if imported.Pos().IsValid() {
							position = imported.Pos()
						}
						t.Errorf("%s: forbidden dependency %q at %v", path, value, position)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", directory, err)
		}
	}
}

func TestDomainDoesNotExposeRawMediaOrUntypedTransport(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{"opencv", "pytorch", "cuda", "protobuf.Any", "map[string]interface{}", "sensor_msgs"}
	err := filepath.WalkDir(filepath.Join(root, "internal/domain"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range forbidden {
			if strings.Contains(strings.ToLower(string(content)), strings.ToLower(value)) {
				t.Errorf("%s contains forbidden core token %q", path, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("go.mod not found")
		}
		directory = parent
	}
}
