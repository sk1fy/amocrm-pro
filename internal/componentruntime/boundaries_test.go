package componentruntime

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDomainDependencyBoundaries(t *testing.T) {
	for _, owner := range []string{"activity", "crmevents"} {
		dir := filepath.Join("..", "services", owner)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, i := range file.Imports {
				p, _ := strconv.Unquote(i.Path.Value)
				for _, forbidden := range []string{"/internal/corepolicy", "/internal/gateway", "/internal/oauth", "/internal/installations", "/internal/integrations", "/internal/jobs", "/internal/widgetauth", "/internal/servicerpc", "/internal/integration/amocrm", "google.golang.org/grpc"} {
					if strings.Contains(p, forbidden) {
						t.Errorf("%s imports forbidden %s", path, p)
					}
				}
				if strings.Contains(p, "/internal/services/") && !strings.Contains(p, "/internal/services/"+owner) {
					t.Errorf("%s imports another service %s", path, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
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
