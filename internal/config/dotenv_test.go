package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvFileSelectsTheDatasourceButRealEnvironmentWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# production database\nexport ZIRNIS_DATASOURCE=postgres\nZIRNIS_DATABASE_URL=\"postgres://zirnis:p#ss@db:5432/zirnis?sslmode=require\"\n\nZIRNIS_MONGODB_DATABASE=evidence # trailing comment\nZIRNIS_ADDR=:9999\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ZIRNIS_DATASOURCE", "ZIRNIS_DATABASE_URL", "ZIRNIS_MONGODB_DATABASE"} {
		t.Setenv(k, "") // registers cleanup
		os.Unsetenv(k)
	}
	t.Setenv("ZIRNIS_ADDR", ":7000") // set by the deployment: must not be overridden
	t.Setenv("ZIRNIS_ENV_FILE", path)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Datasource != "postgres" || c.DatabaseURL != "postgres://zirnis:p#ss@db:5432/zirnis?sslmode=require" || c.MongoDatabase != "evidence" {
		t.Errorf("env file not applied: %q %q %q", c.Datasource, c.DatabaseURL, c.MongoDatabase)
	}
	if c.Addr != ":7000" {
		t.Errorf("the real environment must win over .env, got %s", c.Addr)
	}
}

func TestPublicDemoKeysOnlyWorkInTheSandbox(t *testing.T) {
	t.Setenv("ZIRNIS_ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("ZIRNIS_SANDBOX", "")
	t.Setenv("ZIRNIS_ENV", "Production")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIKeys) != 0 {
		t.Fatalf("demo keys must not authenticate outside the sandbox: %v", c.APIKeys)
	}
	t.Setenv("ZIRNIS_ENV", "Development")
	if c, _ = Load(); len(c.APIKeys) != len(Default().APIKeys) {
		t.Fatal("the sandbox keeps its demo keys")
	}
}

func TestSQLiteIsForTheSandboxUnlessExplicitlyAllowed(t *testing.T) {
	c := Default()
	c.Sandbox = new(bool)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SQLite is for the sandbox") {
		t.Fatalf("expected SQLite to be refused outside the sandbox, got %v", err)
	}
	c.AllowSQLite = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPayerEdgeSettingsAreChecked(t *testing.T) {
	c := Default()
	c.Sandbox, c.AllowSQLite = new(bool), true
	for _, bad := range []string{"http://pay.example.com", "pay.example.com", "https://user:pw@pay.example.com"} {
		c.PublicURL = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ZIRNIS_PUBLIC_URL") {
			t.Fatalf("expected %q to be refused, got %v", bad, err)
		}
	}
	c.PublicURL = "https://pay.example.com"
	c.EdgeTrustedProxies = []string{"10.0.0.0/8", "10.0.0.1"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ZIRNIS_EDGE_TRUSTED_PROXIES") {
		t.Fatalf("expected a bare address to be refused, got %v", err)
	}
	c.EdgeTrustedProxies = []string{"10.0.0.0/8"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
