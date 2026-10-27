// Package storetest gives each test its own empty database on any engine, so the integration
// suite runs unchanged against SQLite, PostgreSQL, MySQL and SQL Server.
//
// Set ZIRNIS_TEST_DATASOURCE (postgres | mysql | sqlserver | oracle) and
// ZIRNIS_TEST_DATABASE_URL — a URL for an account allowed to create databases (Oracle: users) —
// to run against a server engine; otherwise tests use a throwaway SQLite file.
package storetest

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
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
		password := "Mz_" + domain.RandomHex(8)
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

func open(t testing.TB, d store.Dialect, rawURL string) *sql.DB {
	t.Helper()
	dsn, err := store.DSN(d, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(d.DriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// DropTrigger is the engine's statement for removing a trigger (used by tamper tests).
func DropTrigger(d store.Dialect, name, table string) string {
	if d == store.Postgres {
		return fmt.Sprintf("DROP TRIGGER %s ON %s", name, table)
	}
	return "DROP TRIGGER " + strings.TrimSpace(name)
}
