// Package storetest gives each test its own database on any engine, so the integration suite
// runs unchanged against SQLite, PostgreSQL, MySQL, SQL Server and Oracle.
//
// Set ZIRNIS_TEST_DATASOURCE (postgres | mysql | sqlserver | oracle) and
// ZIRNIS_TEST_DATABASE_URL — a URL for an account allowed to create databases (Oracle: users) —
// to run against a server engine; otherwise tests use a throwaway SQLite file.
package storetest

import (
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Datasource is the engine the tests run on.
func Datasource() store.Dialect {
	d, err := store.ParseDialect(os.Getenv("ZIRNIS_TEST_DATASOURCE"))
	if err != nil {
		panic(err)
	}
	return d
}

// FreshDatabase creates an empty database for one test and returns its URL (dropped when the test
// ends). For SQLite it returns "" — the caller uses a temp file.
func FreshDatabase(t testing.TB) (store.Dialect, string) {
	t.Helper()
	d := Datasource()
	if d == store.SQLite {
		return d, ""
	}
	server := os.Getenv("ZIRNIS_TEST_DATABASE_URL")
	if server == "" {
		t.Fatalf("ZIRNIS_TEST_DATABASE_URL is required with ZIRNIS_TEST_DATASOURCE=%s", d)
	}
	name := "zr_" + domain.RandomHex(6)
	admin := open(t, d, server)
	defer admin.Close()

	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	var create, drop []string
	switch d {
	case store.Postgres:
		create = []string{"CREATE DATABASE " + name}
		drop = []string{"DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"}
		u.Path = "/" + name
	case store.MySQL:
		create = []string{"CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"}
		drop = []string{"DROP DATABASE IF EXISTS " + name}
		u.Path = "/" + name
	case store.SQLServer:
		create = []string{"CREATE DATABASE " + name, "ALTER DATABASE " + name + " SET READ_COMMITTED_SNAPSHOT ON"}
		drop = []string{"ALTER DATABASE " + name + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE", "DROP DATABASE " + name}
		q := u.Query()
		q.Set("database", name)
		u.RawQuery = q.Encode()
	case store.Oracle:
		// An Oracle schema is a user: create one per test in the same pluggable database.
		password := "Zr_" + domain.RandomHex(8)
		create = []string{
			fmt.Sprintf(`CREATE USER %s IDENTIFIED BY "%s" QUOTA UNLIMITED ON USERS`, name, password),
			"GRANT CREATE SESSION, CREATE TABLE, CREATE TRIGGER, CREATE SEQUENCE, CREATE PROCEDURE TO " + name,
		}
		drop = []string{"DROP USER " + name + " CASCADE"}
		u.User = url.UserPassword(name, password)
	}
	for _, s := range create {
		if _, err := admin.Exec(s); err != nil {
			t.Fatalf("create test database: %v", err)
		}
	}
	t.Cleanup(func() {
		admin := open(t, d, server)
		defer admin.Close()
		for _, s := range drop {
			_, _ = admin.Exec(s)
		}
	})
	return d, u.String()
}

// Clone gives the test its own copy of a database that build migrated and bootstrapped once for
// every test asking with the same key: on PostgreSQL a CREATE DATABASE … TEMPLATE, on SQLite a
// file copy, each a fraction of the time building it takes. It returns the URL (server engines)
// or the file (SQLite). The other engines can't copy a database cheaply, so their tests get an
// empty one and build it themselves.
func Clone(t testing.TB, key string, build func(d store.Dialect, url, path string) error) (d store.Dialect, databaseURL, path string) {
	t.Helper()
	d = Datasource()
	if d != store.SQLite && d != store.Postgres {
		d, databaseURL = FreshDatabase(t)
		return d, databaseURL, filepath.Join(t.TempDir(), "zirnis-test.db")
	}
	tpl := template(key)
	tpl.once.Do(func() { tpl.err = tpl.build(d, build) })
	if tpl.err != nil {
		t.Fatalf("build the template database: %v", tpl.err)
	}
	if d == store.SQLite {
		path = filepath.Join(t.TempDir(), "zirnis-test.db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := copyFile(tpl.path+suffix, path+suffix); err != nil {
				t.Fatal(err)
			}
		}
		return d, "", path
	}
	server := os.Getenv("ZIRNIS_TEST_DATABASE_URL")
	name := "zr_" + domain.RandomHex(6)
	admin := open(t, d, server)
	defer admin.Close()
	tpl.mu.Lock() // one copy of a template at a time: PostgreSQL refuses to copy one in use
	_, err := admin.Exec("CREATE DATABASE " + name + " TEMPLATE " + tpl.name)
	tpl.mu.Unlock()
	if err != nil {
		t.Fatalf("copy the template database: %v", err)
	}
	t.Cleanup(func() {
		admin := open(t, d, server)
		defer admin.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return d, u.String(), filepath.Join(t.TempDir(), "zirnis-test.db")
}

type templateDB struct {
	once sync.Once
	mu   sync.Mutex
	name string // PostgreSQL
	path string // SQLite
	err  error
}

var templates = struct {
	sync.Mutex
	byKey map[string]*templateDB
	dir   string
}{byKey: map[string]*templateDB{}}

func template(key string) *templateDB {
	templates.Lock()
	defer templates.Unlock()
	tpl := templates.byKey[key]
	if tpl == nil {
		tpl = &templateDB{}
		templates.byKey[key] = tpl
	}
	return tpl
}

func (tpl *templateDB) build(d store.Dialect, build func(d store.Dialect, url, path string) error) error {
	if d == store.SQLite {
		templates.Lock()
		if templates.dir == "" {
			dir, err := os.MkdirTemp("", "zirnis-templates-")
			if err != nil {
				templates.Unlock()
				return err
			}
			templates.dir = dir
		}
		tpl.path = filepath.Join(templates.dir, domain.RandomHex(6)+".db")
		templates.Unlock()
		return build(d, "", tpl.path)
	}
	server := os.Getenv("ZIRNIS_TEST_DATABASE_URL")
	if server == "" {
		return fmt.Errorf("ZIRNIS_TEST_DATABASE_URL is required with ZIRNIS_TEST_DATASOURCE=%s", d)
	}
	admin, err := openDB(d, server)
	if err != nil {
		return err
	}
	defer admin.Close()
	tpl.name = "zr_template_" + domain.RandomHex(6)
	if _, err := admin.Exec("CREATE DATABASE " + tpl.name); err != nil {
		return err
	}
	u, err := url.Parse(server)
	if err != nil {
		return err
	}
	u.Path = "/" + tpl.name
	return build(d, u.String(), "")
}

// DropTemplates removes the template databases once the tests are done (TestMain).
func DropTemplates() {
	templates.Lock()
	defer templates.Unlock()
	if templates.dir != "" {
		_ = os.RemoveAll(templates.dir)
	}
	var admin *sql.DB
	for _, tpl := range templates.byKey {
		if tpl.name == "" {
			continue
		}
		if admin == nil {
			var err error
			if admin, err = openDB(Datasource(), os.Getenv("ZIRNIS_TEST_DATABASE_URL")); err != nil {
				return
			}
			defer admin.Close()
		}
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + tpl.name + " WITH (FORCE)")
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func open(t testing.TB, d store.Dialect, rawURL string) *sql.DB {
	t.Helper()
	db, err := openDB(d, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func openDB(d store.Dialect, rawURL string) (*sql.DB, error) {
	dsn, err := store.DSN(d, rawURL)
	if err != nil {
		return nil, err
	}
	return sql.Open(d.DriverName(), dsn)
}

// DropTrigger is the engine's statement for removing a trigger (used by tamper tests).
func DropTrigger(d store.Dialect, name, table string) string {
	if d == store.Postgres {
		return fmt.Sprintf("DROP TRIGGER %s ON %s", name, table)
	}
	return "DROP TRIGGER " + strings.TrimSpace(name)
}
