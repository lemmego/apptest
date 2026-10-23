package apptest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"
)

// WithScratchDB gives the test its own database, migrated and then dropped.
//
// It creates a uniquely named database on the server the application is
// already configured to use, points the configuration at it, runs every
// migration registered with migration.GetMigrator, and drops it when the test
// ends. Tests therefore do not see each other's rows and can run in any
// order.
//
// **The server comes from the application's own configuration**, which under
// a test binary means .env.test — see api/config. There is no separate
// environment variable naming a database, and that is the point: two ways to
// say which server to use is two things that can disagree. The previous
// APPTEST_POSTGRES_DSN did disagree, in a way worth recording, because it
// supplied host, user, password and port but not options — so a managed URL's
// sslmode=require survived into a local test server and every test panicked
// with "pq: SSL is not enabled on the server". Reading one source cannot
// produce that.
//
// **It fails rather than skipping.** The old behaviour skipped when the
// variable was unset, reasoning that `go test ./...` should not fail for
// somebody with no server running. The cost of that kindness was a suite
// reporting ok in 1.3 seconds having skipped 448 tests and run 7 — a pass
// indistinguishable from a real one except by the clock. A test that needs a
// database and has none is not a test that passed.
//
// A scratch database per test is deliberately heavier than a transaction
// rolled back at the end. It is also the only thing that tells the truth
// about a migration, a trigger, a generated column or a deferred constraint,
// which is most of what is worth testing at this layer.
//
// Scratch databases are implemented for Postgres only. The name does not say
// so because the contract is not Postgres-shaped — an application configured
// for another engine gets a clear failure rather than an option it was never
// allowed to call.
func WithScratchDB() Option {
	return func(o *options) {
		o.postgres = true
		o.migrate = true
	}
}

// WithScratchDBNoMigrate is WithScratchDB without running migrations, for a
// test that wants to exercise the migrator itself.
func WithScratchDBNoMigrate() Option {
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

	adminDSN := adminDSNFromConfig(a.t, o.config)

	scratch, err := createScratch(adminDSN)
	if err != nil {
		a.t.Fatalf("apptest: %v", err)
	}
	a.scratch = scratch

	// Register the drop the instant the database exists, before anything that
	// can fail. Everything below — pointing the config at it, running
	// migrations — calls t.Fatalf on failure, and Fatalf calls runtime.Goexit,
	// so a cleanup registered after this point would never be reached. A
	// failing migration would then leave a database behind on every run, and
	// the developer fixing the migration accumulates one per attempt.
	//
	// createScratch has already registered it for the interrupt handler,
	// which is the half t.Cleanup cannot do.
	a.t.Cleanup(a.tearDownPostgres)

	pointConfigAt(a.t, o.config, adminDSN, scratch.name)

	if o.migrate {
		a.runMigrations(dsnForDatabase(adminDSN, scratch.name))
	}
}

// createScratch creates a uniquely named database and records it, so that an
// interrupt can drop it even though t.Cleanup will not run.
//
// Registration happens here rather than at the call site because it must
// happen in the same breath as the CREATE: a database that exists and is not
// registered is one nothing will ever remove.
func createScratch(adminDSN string) (*scratchDB, error) {
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		return nil, fmt.Errorf("opening the configured database server: %w", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil, fmt.Errorf("the configured database server is unreachable: %w", err)
	}

	name := scratchName()
	// The identifier is generated here, not supplied, so interpolating it is
	// safe — CREATE DATABASE takes no parameters anyway.
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		return nil, fmt.Errorf("creating database %s: %w", name, err)
	}

	scratch := &scratchDB{adminDSN: adminDSN, name: name}
	registerScratch(scratch)
	return scratch, nil
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
	// Forget it first. A failed drop is reported and left behind either way,
	// and the interrupt handler retrying a drop that has already failed once
	// would only delay the exit it is running inside.
	unregisterScratch(a.scratch)
	if err := dropScratch(a.scratch); err != nil {
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
		t.Fatalf("apptest: WithScratchDB needs the application's sql config; pass WithConfig(config.GetAll())")
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
		t.Fatalf("apptest: sql.connections.%s is missing; WithScratchDB needs a connection block", selected)
		return
	}
	if driver, _ := conn["driver"].(string); driver != migration.DriverPostgres {
		t.Fatalf("apptest: sql.connections.%s has driver %q; scratch databases are implemented for postgres only", selected, driver)
		return
	}

	// Only the database name changes. Host, user, password, port and every
	// option are already the application's own, because that is where the
	// scratch database was created — so there is nothing to copy across and
	// nothing that can disagree. The old version rewrote four of those from
	// an environment DSN and left options alone, which is how a managed URL's
	// sslmode=require reached a local server.
	settings := map[string]any{"database": name}

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

// parseDSN reads a Postgres DSN in either of the two forms lib/pq accepts and
// returns it as keyword settings.
//
// Only the keyword form was handled, and a URL was not rejected — it was
// mangled. strings.Fields on "postgres://you@host:5432/db?sslmode=disable"
// yields one field, Cut on "=" splits it at the query parameter, and the
// result was a settings map keyed on the whole URL up to "sslmode". The error
// that came back named the symptom and not the cause:
//
//	pq: unsupported sslmode "disable dbname=apptest_20a89edc8248"
//
// URL form is what anybody writes in CI, and what every other tool in this
// repo accepts, so it is supported rather than diagnosed.
func parseDSN(dsn string) map[string]string {
	if parsed, err := url.Parse(dsn); err == nil &&
		(parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		return urlDSNSettings(parsed)
	}

	parts := map[string]string{}
	for _, field := range strings.Fields(dsn) {
		key, value, found := strings.Cut(field, "=")
		if found {
			parts[key] = value
		}
	}
	return parts
}

// urlDSNSettings flattens a postgres:// URL into keyword settings.
func urlDSNSettings(parsed *url.URL) map[string]string {
	parts := map[string]string{}
	if host := parsed.Hostname(); host != "" {
		parts["host"] = host
	}
	if port := parsed.Port(); port != "" {
		parts["port"] = port
	}
	if parsed.User != nil {
		if user := parsed.User.Username(); user != "" {
			parts["user"] = user
		}
		if password, ok := parsed.User.Password(); ok {
			parts["password"] = password
		}
	}
	if name := strings.TrimPrefix(parsed.Path, "/"); name != "" {
		parts["dbname"] = name
	}
	// Query parameters last, so sslmode and the rest survive. Only the first
	// value of each: lib/pq takes one, and a DSN repeating a parameter is a
	// mistake rather than a list.
	for key, values := range parsed.Query() {
		if len(values) > 0 && values[0] != "" {
			parts[key] = values[0]
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

// adminDSNFromConfig builds a DSN for the server the application is
// configured to use.
//
// "Administrative" only in the sense that it must be able to CREATE DATABASE;
// it is the application's own connection, pointed at the database its config
// names. The scratch databases are created beside that one.
//
// This replaces an environment variable, and the reason is that the
// application already knows the answer. Asking twice invites the two answers
// to differ, which is exactly what happened before: the variable carried
// host, user, password and port, the config carried options, and a managed
// URL's sslmode=require reached a server that had no TLS.
func adminDSNFromConfig(t testingT, cfg config.M) string {
	t.Helper()

	block, ok := cfg["sql"].(config.M)
	if !ok {
		t.Fatalf("apptest: WithScratchDB needs the application's sql config; pass WithConfig(config.GetAll())")
		return ""
	}
	selected, _ := block["default"].(string)
	if selected == "" {
		t.Fatalf("apptest: sql.default is not set, so there is no server to create a scratch database on")
		return ""
	}
	connections, ok := block["connections"].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections is missing from the configuration")
		return ""
	}
	conn, ok := connections[selected].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections.%s is missing", selected)
		return ""
	}
	if driver, _ := conn["driver"].(string); driver != migration.DriverPostgres {
		t.Fatalf("apptest: sql.connections.%s has driver %q; scratch databases are implemented for postgres only",
			selected, driver)
		return ""
	}

	host, _ := conn["host"].(string)
	database, _ := conn["database"].(string)
	if host == "" || database == "" {
		// Named explicitly rather than left to fail at connect time, because
		// "dial tcp :5432" tells somebody nothing about which file is wrong.
		t.Fatalf("apptest: sql.connections.%s needs host and database; under a test binary these come from %s",
			selected, config.DotEnvTestFile)
		return ""
	}

	parts := []string{"host=" + host, "dbname=" + database}
	if user, _ := conn["user"].(string); user != "" {
		parts = append(parts, "user="+user)
	}
	if password, _ := conn["password"].(string); password != "" {
		parts = append(parts, "password="+password)
	}
	switch port := conn["port"].(type) {
	case int:
		if port > 0 {
			parts = append(parts, "port="+strconv.Itoa(port))
		}
	case string:
		if port != "" {
			parts = append(parts, "port="+port)
		}
	}
	// Options last, so a connection block that sets sslmode keeps it. This is
	// the field the environment variable never carried.
	if options, _ := conn["options"].(config.M); options != nil {
		for key, value := range options {
			if text, ok := value.(string); ok && text != "" {
				parts = append(parts, key+"="+text)
			}
		}
	}
	if params, _ := conn["params"].(string); params != "" {
		for _, pair := range strings.Split(params, "&") {
			if pair != "" {
				parts = append(parts, strings.ReplaceAll(pair, "=", "="))
			}
		}
	}
	return strings.Join(parts, " ")
}
