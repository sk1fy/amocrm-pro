package componentruntime

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every new product directory is checked automatically. leadstatus is the
// explicitly existing Core-owned module, not a separately hosted product.
func domainDependencyViolations(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "leadstatus" || entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		owner := entry.Name()
		err := filepath.WalkDir(filepath.Join(root, owner), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				// net/url is pure data handling; net itself permits direct sockets.
				// The root services package also owns Core SQL facilities/catalog.
				if importPath == "net" || importPath == "github.com/sk1fy/amocrm-pro/internal/services" {
					violations = append(violations, fmt.Sprintf("%s imports forbidden %s", path, importPath))
				}
				for _, forbidden := range []string{
					"github.com/sk1fy/amocrm-pro/internal/corepolicy",
					"github.com/sk1fy/amocrm-pro/internal/gateway",
					"github.com/sk1fy/amocrm-pro/internal/oauth",
					"github.com/sk1fy/amocrm-pro/internal/installations",
					"github.com/sk1fy/amocrm-pro/internal/integrations",
					"github.com/sk1fy/amocrm-pro/internal/jobs",
					"github.com/sk1fy/amocrm-pro/internal/widgetauth",
					"github.com/sk1fy/amocrm-pro/internal/widgetapi",
					"github.com/sk1fy/amocrm-pro/internal/widgetcors",
					"github.com/sk1fy/amocrm-pro/internal/widgetlimit",
					"github.com/sk1fy/amocrm-pro/internal/activitybridge",
					"github.com/sk1fy/amocrm-pro/internal/componentruntime",
					"github.com/sk1fy/amocrm-pro/internal/servicerpc",
					"github.com/sk1fy/amocrm-pro/internal/integration/amocrm",
					"github.com/sk1fy/amocrm-pro/internal/platform/config",
					"github.com/sk1fy/amocrm-pro/internal/platform/postgres",
					"github.com/sk1fy/amocrm-pro/internal/transport",
					"google.golang.org/grpc", "net/http", "os/exec",
				} {
					if importPath == forbidden || strings.HasPrefix(importPath, forbidden+"/") {
						violations = append(violations, fmt.Sprintf("%s imports forbidden %s", path, importPath))
					}
				}
				const servicesPrefix = "github.com/sk1fy/amocrm-pro/internal/services/"
				if strings.HasPrefix(importPath, servicesPrefix) {
					ownPackage := servicesPrefix + owner
					if importPath != ownPackage && !strings.HasPrefix(importPath, ownPackage+"/") {
						violations = append(violations, fmt.Sprintf("%s imports another service %s", path, importPath))
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return violations, nil
}

func TestDomainDependencyBoundaries(t *testing.T) {
	violations, err := domainDependencyViolations("../services")
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}

	entries, err := os.ReadDir("../serviceapi")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../serviceapi", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"pgx", "pgxpool", "database/sql", "internal/servicerpc", "internal/services/"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("domain contracts contain %s", forbidden)
			}
		}
	}
}

func TestNewProductDirectoriesCannotBypassBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, dependency string
		forbidden        bool
	}{
		{"foreign_repository", "github.com/sk1fy/amocrm-pro/internal/services/activity", true},
		{"similar_name_is_not_own", "github.com/sk1fy/amocrm-pro/internal/services/reportsarchive", true},
		{"credentials", "github.com/sk1fy/amocrm-pro/internal/oauth", true},
		{"core_queue", "github.com/sk1fy/amocrm-pro/internal/jobs", true},
		{"core_catalog_store", "github.com/sk1fy/amocrm-pro/internal/services", true},
		{"raw_client", "github.com/sk1fy/amocrm-pro/internal/integration/amocrm", true},
		{"direct_http", "net/http", true},
		{"direct_socket", "net", true},
		{"composition", "github.com/sk1fy/amocrm-pro/internal/componentruntime", true},
		{"contracts", "github.com/sk1fy/amocrm-pro/internal/serviceapi", false},
		{"url_value", "net/url", false},
		{"own_adapter", "github.com/sk1fy/amocrm-pro/internal/services/reports/storage", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "reports")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source := fmt.Sprintf("package reports\nimport _ %q\n", tt.dependency)
			if err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			violations, err := domainDependencyViolations(root)
			if err != nil {
				t.Fatal(err)
			}
			if (len(violations) > 0) != tt.forbidden {
				t.Fatalf("new product dependency %s: violations=%v", tt.dependency, violations)
			}
		})
	}
}

func TestStandaloneRejectsForeignConfiguration(t *testing.T) {
	for _, key := range []string{"DATABASE_URL", "ENCRYPTION_KEYS", "AMOCRM_CLIENT_SECRET", "ACTIVITY_DATABASE_URL", "CRM_EVENTS_DATABASE_URL", "ACTIVITY_MODE"} {
		t.Setenv(key, "")
	}
	t.Setenv("ACTIVITY_MODE", "grpc")
	t.Setenv("ACTIVITY_DATABASE_URL", "postgres://activity_runtime@localhost/activity")
	if _, err := Load("activity"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"DATABASE_URL", "ENCRYPTION_KEYS", "AMOCRM_CLIENT_SECRET", "CRM_EVENTS_DATABASE_URL"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "forbidden")
			if _, err := Load("activity"); err == nil {
				t.Errorf("accepted foreign %s", key)
			}
		})
	}
	t.Setenv("ACTIVITY_DATABASE_URL", "")
	t.Setenv("CRM_EVENTS_DATABASE_URL", "postgres://events_runtime@localhost/events")
	if _, err := Load("crm-events"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACTIVITY_DATABASE_URL", "foreign")
	if _, err := Load("crm-events"); err == nil {
		t.Fatal("events accepted Activity DSN")
	}
	if _, err := Load("api"); err == nil {
		t.Fatal("grpc Core accepted product DSNs")
	}
}
