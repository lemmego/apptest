package apptest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"
)

// PostgresDSNEnv names the environment variable holding an administrative
// Postgres DSN — one that can CREATE DATABASE. Point it at any database on
// the server; the scratch databases are created beside it.
//
//	APPTEST_POSTGRES_DSN="host=localhost user=you dbname=postgres sslmode=disable"
const PostgresDSNEnv = "APPTEST_POSTGRES_DSN"

// WithPostgres gives the test its own Postgres database, migrated and then
// dropped.
//
// It creates a uniquely named database, rewrites the configuration the
// application boots with so its connector opens that one, runs every
// migration registered with migration.GetMigrator, and drops it when the test
// ends. Tests therefore do not see each other's rows and can run in any
// order.
//
// It skips the test when APPTEST_POSTGRES_DSN is unset, rather than failing.
// A repository test is worth nothing against a fake, and worth a great deal
// against the real thing — but it must not make `go test ./...` fail for
// somebody who has no server running.
//
// A scratch database per test is deliberately heavier than a transaction
// rolled back at the end. It is also the only thing that tells the truth
// about a migration, a trigger, a generated column or a deferred constraint,
// which is most of what is worth testing at this layer.
func WithPostgres() Option {
	return func(o *options) {
		o.postgres = true
		o.migrate = true
	}
}

// WithPostgresNoMigrate is WithPostgres without running migrations, for a test
// that wants to exercise the migrator itself.
func WithPostgresNoMigrate() Option {
	return func(o *options) {
		o.postgres = true
		o.migrate = false
	}
}

type scratchDB struct {
	adminDSN string
	name     string
}

func (a *App) setUpPostgres(o *options) {
	a.t.Helper()

	adminDSN := os.Getenv(PostgresDSNEnv)
	if adminDSN == "" {
		a.t.Skipf("apptest: set %s to run tests that need a database", PostgresDSNEnv)
	}

	name := scratchName()

	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		a.t.Fatalf("apptest: opening %s: %v", PostgresDSNEnv, err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		a.t.Fatalf("apptest: %s is set but unreachable: %v", PostgresDSNEnv, err)
	}

	// The identifier is generated here, not supplied, so interpolating it is
	// safe — CREATE DATABASE takes no parameters anyway.
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		a.t.Fatalf("apptest: creating database %s: %v", name, err)
	}

	a.scratch = &scratchDB{adminDSN: adminDSN, name: name}

	// Register the drop the instant the database exists, before anything that
	// can fail. Everything below — pointing the config at it, running
	// migrations — calls t.Fatalf on failure, and Fatalf calls runtime.Goexit,
	// so a cleanup registered after this point would never be reached. A
	// failing migration would then leave a database behind on every run, and
	// the developer fixing the migration accumulates one per attempt.
	a.t.Cleanup(a.tearDownPostgres)

	pointConfigAt(a.t, o.config, adminDSN, name)

	if o.migrate {
		a.runMigrations(dsnForDatabase(adminDSN, name))
	}
}

func (a *App) runMigrations(dsn string) {
	a.t.Helper()

	// migration.Create resolves its dialect from the DB_DRIVER environment
	// variable at the moment a migration's Up runs, defaulting to sqlite when
	// it is unset — and a project's .env is loaded relative to the working
	// directory, which under `go test ./internal/...` is the package
	// directory, not the project root. So a migration written with the
	// dialect-agnostic builder emits SQLite DDL and Postgres rejects it with
	// a syntax error near AUTOINCREMENT.
	//
	// The harness knows which database it just created, so it says so.
	// t.Setenv restores the previous value and refuses to run in a parallel
	// test, which is the correct constraint for a process-global variable.
	a.t.Setenv("DB_DRIVER", migration.DriverPostgres)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		a.t.Fatalf("apptest: opening the scratch database: %v", err)
	}
	defer db.Close()

	m, err := migration.Init(db, migration.DriverPostgres)
	if err != nil {
		a.t.Fatalf("apptest: initialising the migrator: %v", err)
	}
	// -1 runs everything pending, which is what a fresh database needs.
	if err := m.Up(-1); err != nil {
		a.t.Fatalf("apptest: running migrations: %v", err)
	}
}

func (a *App) tearDownPostgres() {
	if a.scratch == nil {
		return
	}
	admin, err := sql.Open("postgres", a.scratch.adminDSN)
	if err != nil {
		a.t.Logf("apptest: could not reopen %s to drop %s: %v", PostgresDSNEnv, a.scratch.name, err)
		return
	}
	defer admin.Close()
	// FORCE terminates connections the application left open; without it a
	// pool that has not finished closing keeps the database alive and every
	// subsequent run leaks another one.
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS "` + a.scratch.name + `" WITH (FORCE)`); err != nil {
		a.t.Logf("apptest: dropping %s: %v — drop it by hand", a.scratch.name, err)
	}
	a.scratch = nil
}

// pointConfigAt rewrites the sql block so the application's connector opens
// the scratch database.
//
// It writes to two places, and both are necessary.
//
// The configuration map the test passed, because that is what the application
// boots with — and it edits that map rather than the environment because a
// project's internal/configs package reads the environment in init(), which
// has already run by the time a test calls New.
//
// And the package-level config singleton, because the connectors do not read
// the application's configuration instance: ormconnector.sqlConfig and its
// siblings call the package-level config.Get, which resolves the global.
// Setting only the application's copy leaves the connector opening whatever
// the environment said, which under `go test ./internal/...` is nothing at
// all, since .env is loaded relative to the working directory.
func pointConfigAt(t testingT, cfg config.M, adminDSN, name string) {
	t.Helper()

	block, ok := cfg["sql"].(config.M)
	if !ok {
		t.Fatalf("apptest: WithPostgres needs the application's sql config; pass WithConfig(config.GetAll())")
		return
	}
	connections, ok := block["connections"].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections is missing from the configuration")
		return
	}

	selected, _ := block["default"].(string)
	if selected == "" {
		t.Fatalf("apptest: sql.default is not set, so there is no connection to point at the scratch database")
		return
	}
	conn, ok := connections[selected].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections.%s is missing; WithPostgres needs a postgres connection block", selected)
		return
	}
	if driver, _ := conn["driver"].(string); driver != migration.DriverPostgres {
		t.Fatalf("apptest: sql.connections.%s has driver %q; WithPostgres needs the default connection to be postgres", selected, driver)
		return
	}

	admin := parseDSN(adminDSN)
	settings := map[string]any{"database": name}
	if host, ok := admin["host"]; ok {
		settings["host"] = host
	}
	if user, ok := admin["user"]; ok {
		settings["user"] = user
	}
	if password, ok := admin["password"]; ok {
		settings["password"] = password
	}
	// The connectors assert the port to an int, so a string here resolves to
	// the zero value and the connection quietly goes to the driver's default
	// port instead of the one the DSN named.
	if port, ok := admin["port"]; ok {
		if n, err := strconv.Atoi(port); err == nil {
			settings["port"] = n
		}
	}

	prefix := "sql.connections." + selected + "."
	for key, value := range settings {
		conn[key] = value
		config.Set(prefix+key, value)
	}
	config.Set("sql.default", selected)
}

// dsnForDatabase rewrites dbname in a key=value Postgres DSN.
func dsnForDatabase(dsn, name string) string {
	parts := parseDSN(dsn)
	parts["dbname"] = name
	pairs := make([]string, 0, len(parts))
	for key, value := range parts {
		pairs = append(pairs, key+"="+value)
	}
	return strings.Join(pairs, " ")
}

func parseDSN(dsn string) map[string]string {
	parts := map[string]string{}
	for _, field := range strings.Fields(dsn) {
		key, value, found := strings.Cut(field, "=")
		if found {
			parts[key] = value
		}
	}
	return parts
}

func scratchName() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// Only reached if the OS entropy source fails, in which case a
		// predictable name is still better than no database.
		return fmt.Sprintf("apptest_%d", os.Getpid())
	}
	return "apptest_" + hex.EncodeToString(buf)
}

// sprintf is fmt.Sprintf, named locally so the test recorder can format a
// message without importing fmt into a file that otherwise needs it only
// here.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// testingT is the slice of *testing.T this file needs, so pointConfigAt can be
// tested without a live server.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}
