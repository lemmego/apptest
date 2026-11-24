package apptest

import (
	"database/sql"
	"slices"
	"sort"
	"strings"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"
)

// DriverLibSQL is the driver name github.com/tursodatabase/libsql-client-go
// registers with database/sql.
const DriverLibSQL = "libsql"

// WithScratchLibSQL gives the test a migrated, empty libSQL database.
//
// It connects to the server the application is already configured to use,
// applies every migration registered with migration.GetMigrator, and empties
// every table the migrations created. Each test therefore starts from a known
// schema with no rows, which is the same contract WithScratchDB offers on
// Postgres.
//
// # Why this empties tables instead of creating a scratch database
//
// Because neither of the alternatives survives contact with a test suite.
//
// A libSQL server has no CREATE DATABASE — a sqld process owns one data
// directory. It does have namespaces, enabled with --enable-namespaces and
// created through the admin API, and they are genuinely isolated. But a client
// selects a namespace by the FIRST LABEL OF THE HOST, which means a test would
// have to reach the server as "scratch-abc123.somehost". Measured on macOS:
// *.localhost does not resolve (curl special-cases it per RFC 6761, Go's
// resolver does not), so the harness would work on Linux, fail on a developer
// machine, and the failure would look like a broken server rather than a DNS
// assumption. Setting a Host header does not help either: net/http ignores
// Header["Host"] and reads Request.Host, which the libSQL client does not
// expose.
//
// So: one database, emptied between tests. The cost is that tests sharing a
// server cannot run in parallel — which t.Setenv already forbids in the
// Postgres path for the same kind of reason.
//
// # Why it empties before rather than after
//
// A failed test leaves its rows behind for inspection, and the next test
// cleans up rather than inheriting them. Emptying afterwards would destroy the
// evidence at exactly the moment somebody wants it.
func WithScratchLibSQL() Option {
	return func(o *options) {
		o.libsql = true
		o.migrate = true
	}
}

// WithScratchLibSQLNoMigrate is WithScratchLibSQL without running migrations,
// for a test that wants to exercise the migrator itself.
func WithScratchLibSQLNoMigrate() Option {
	return func(o *options) {
		o.libsql = true
		o.migrate = false
	}
}

func (a *App) setUpLibSQL(o *options) {
	a.t.Helper()

	if !slices.Contains(sql.Drivers(), DriverLibSQL) {
		a.t.Fatalf("apptest: WithScratchLibSQL needs a %q driver registered; "+
			`blank-import _ "github.com/tursodatabase/libsql-client-go/libsql" `+
			"in the test package. This harness imports no driver, for the reason "+
			"sqlite.go records.", DriverLibSQL)
		return
	}

	url := libsqlURLFromConfig(a.t, o.config)

	sqlDB, err := sql.Open(DriverLibSQL, url)
	if err != nil {
		a.t.Fatalf("apptest: opening %s: %v", url, err)
	}
	defer sqlDB.Close()
	if err := sqlDB.Ping(); err != nil {
		a.t.Fatalf("apptest: the configured libSQL server is unreachable at %s: %v", url, err)
	}

	// Migrations first, so the tables to empty exist. Idempotent after the
	// first test in the package: schema_migrations records them, so Up is a
	// no-op rather than a cost paid per test.
	if o.migrate {
		a.runLibSQLMigrations(sqlDB)
	}
	a.emptyLibSQLTables(sqlDB)
}

func (a *App) runLibSQLMigrations(sqlDB *sql.DB) {
	a.t.Helper()

	// libSQL is a fork of SQLite, so the DDL is SQLite's. migration.Create
	// resolves its dialect from DB_DRIVER when a migration's Up runs, and
	// NormaliseDialect maps "libsql" onto sqlite — but this says so outright
	// rather than relying on the project's .env being found, which under
	// `go test ./internal/...` it is not.
	a.t.Setenv("DB_DRIVER", migration.DriverSQLite)

	m, err := migration.Init(sqlDB, DriverLibSQL)
	if err != nil {
		a.t.Fatalf("apptest: initialising the migrator: %v", err)
	}
	if err := m.Up(-1); err != nil {
		a.t.Fatalf("apptest: running migrations: %v", err)
	}
}

// emptyLibSQLTables deletes every row from every table the migrations created.
//
// schema_migrations is left alone deliberately: emptying it would make the
// next test re-run every migration, turning a no-op into the most expensive
// thing in the suite. sqlite_% is skipped because sqlite_sequence and friends
// are the engine's, not the application's.
func (a *App) emptyLibSQLTables(sqlDB *sql.DB) {
	a.t.Helper()

	rows, err := sqlDB.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name <> 'schema_migrations' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		a.t.Fatalf("apptest: listing tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			a.t.Fatalf("apptest: listing tables: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		a.t.Fatalf("apptest: listing tables: %v", err)
	}
	rows.Close()

	// Sorted so the statements are issued in a stable order, which makes a
	// foreign-key failure reproducible rather than dependent on how
	// sqlite_master happened to be scanned.
	sort.Strings(tables)

	// Foreign keys are suspended for the duration, because there is no
	// deletion order that satisfies a cycle, and a test fixture is entitled to
	// have one. PRAGMA is per-connection, and this is the harness's own
	// connection rather than the application's.
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		a.t.Logf("apptest: could not suspend foreign keys (%v); deleting anyway", err)
	}
	for _, table := range tables {
		// The name comes from sqlite_master, not from a caller, and DELETE
		// takes no parameter for a table name.
		if _, err := sqlDB.Exec(`DELETE FROM "` + table + `"`); err != nil {
			a.t.Fatalf("apptest: emptying %s: %v", table, err)
		}
	}
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		a.t.Logf("apptest: could not restore foreign keys: %v", err)
	}
}

// libsqlURLFromConfig reads the server's address out of the application's own
// configuration, which is where WithScratchDB reads its server from too. There
// is deliberately no environment variable: two ways to say which server to use
// is two things that can disagree, and postgres.go records what that cost last
// time.
func libsqlURLFromConfig(t testingT, cfg config.M) string {
	t.Helper()

	block, ok := cfg["sql"].(config.M)
	if !ok {
		t.Fatalf("apptest: WithScratchLibSQL needs the application's sql config; pass WithConfig(config.GetAll())")
		return ""
	}
	connections, ok := block["connections"].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections is missing from the configuration")
		return ""
	}
	selected, _ := block["default"].(string)
	if selected == "" {
		t.Fatalf("apptest: sql.default is not set, so there is no connection to use")
		return ""
	}
	conn, ok := connections[selected].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections.%s is missing", selected)
		return ""
	}
	if driver, _ := conn["driver"].(string); !strings.EqualFold(driver, DriverLibSQL) {
		t.Fatalf("apptest: sql.connections.%s has driver %q; WithScratchLibSQL is for %q",
			selected, driver, DriverLibSQL)
		return ""
	}
	url, _ := conn["url"].(string)
	if url == "" {
		t.Fatalf("apptest: sql.connections.%s needs a url; a libSQL server's address is "+
			"a URL such as http://127.0.0.1:8080, which host and port cannot express", selected)
		return ""
	}
	return url
}
