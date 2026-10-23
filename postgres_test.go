package apptest

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"
)

// postgresConfig is the configuration an application under test would have.
//
// It reads libpq's own variables rather than one of apptest's, because this
// package no longer has one — the application's configuration is the single
// source of where the database is, and these tests stand in for an
// application. PGHOST and friends are what psql, pg_dump and every other
// client already read, so a developer with a server running usually needs to
// set nothing.
func postgresConfig() config.M {
	port := 5432
	if text := os.Getenv("PGPORT"); text != "" {
		if n, err := strconv.Atoi(text); err == nil {
			port = n
		}
	}
	return config.M{
		"app": config.M{"name": "apptest", "env": "test", "port": 0},
		"sql": config.M{
			"default": "postgres",
			"connections": config.M{
				"postgres": config.M{
					"driver":   "postgres",
					"host":     envOr("PGHOST", "localhost"),
					"port":     port,
					"database": envOr("PGDATABASE", "postgres"),
					"user":     envOr("PGUSER", os.Getenv("USER")),
					"password": os.Getenv("PGPASSWORD"),
					"options":  config.M{"sslmode": envOr("PGSSLMODE", "disable")},
				},
			},
		},
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// skipWithoutPostgres skips this package's own tests when no server is
// reachable.
//
// A skip here is a different decision from the one WithScratchDB used to
// make. That skip ran in other people's suites, where a test needing a
// database and silently not running reads as a pass. This one runs in
// apptest's own, where the CI that matters has a postgres service and a
// contributor without one is not owed a failure.
func skipWithoutPostgres(t *testing.T) {
	t.Helper()
	db, err := sql.Open("postgres", testAdminDSN(t))
	if err != nil {
		t.Skipf("apptest: no postgres reachable (%v); set PGHOST, PGUSER and PGDATABASE to run these", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("apptest: no postgres reachable (%v); set PGHOST, PGUSER and PGDATABASE to run these", err)
	}
}

// WithScratchDB has to leave the application pointed at the scratch database,
// not at whatever the project's .env happens to say. Getting this wrong means
// a test suite quietly writing into a developer's working database.
func TestWithScratchDBCreatesAndMigratesAScratchDatabase(t *testing.T) {
	skipWithoutPostgres(t)
	oldMigrator := migration.GetMigrator()
	t.Cleanup(func() { _ = oldMigrator })

	migration.GetMigrator().AddMigration(&migration.Migration{
		Version: "20260201000000",
		Up: func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE apptest_probe (id BIGSERIAL PRIMARY KEY, note TEXT)`)
			return err
		},
		Down: func(tx *sql.Tx) error {
			_, err := tx.Exec(`DROP TABLE apptest_probe`)
			return err
		},
	})

	cfg := postgresConfig()
	a := New(t, WithConfig(cfg), WithScratchDB())

	// The configuration the application booted with now names the scratch
	// database, and it is not the placeholder.
	conn := cfg["sql"].(config.M)["connections"].(config.M)["postgres"].(config.M)
	name, _ := conn["database"].(string)
	if !strings.HasPrefix(name, "apptest_") {
		t.Fatalf("sql.connections.postgres.database = %q, want a scratch name", name)
	}
	if a.scratch == nil || a.scratch.name != name {
		t.Fatalf("scratch database = %+v, config says %q", a.scratch, name)
	}

	// The migration ran, in that database.
	db, err := sql.Open("postgres", dsnForDatabase(a.scratch.adminDSN, name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n int
	err = db.QueryRow(`SELECT count(*) FROM information_schema.tables
	                   WHERE table_schema = current_schema() AND table_name = 'apptest_probe'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("migrations did not run against the scratch database")
	}
}

// Two applications in one package must not share a database, or tests see
// each other's rows and start depending on order.
func TestWithScratchDBIsolatesEachApp(t *testing.T) {
	skipWithoutPostgres(t)
	first := New(t, WithConfig(postgresConfig()), WithScratchDBNoMigrate())
	second := New(t, WithConfig(postgresConfig()), WithScratchDBNoMigrate())

	if first.scratch.name == second.scratch.name {
		t.Fatalf("both apps got %q", first.scratch.name)
	}
}

// pointConfigAt is the part most likely to be given a configuration it cannot
// use, and the failure has to name the problem rather than panic on a type
// assertion.
func TestPointConfigAtRejectsUnusableConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.M
		want string
	}{
		{"no sql block", config.M{}, "pass WithConfig"},
		{"no connections", config.M{"sql": config.M{"default": "postgres"}}, "sql.connections is missing"},
		{"no default", config.M{"sql": config.M{"connections": config.M{}}}, "sql.default is not set"},
		{
			"default names a missing connection",
			config.M{"sql": config.M{"default": "pg", "connections": config.M{}}},
			"sql.connections.pg is missing",
		},
		{
			"default is not postgres",
			config.M{"sql": config.M{"default": "mysql", "connections": config.M{
				"mysql": config.M{"driver": "mysql"},
			}}},
			"implemented for postgres only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &fatalRecorder{}
			pointConfigAt(probe, tc.cfg, "host=h user=u dbname=d", "apptest_x")
			if probe.message == "" {
				t.Fatal("accepted a configuration it cannot use")
			}
			if !strings.Contains(probe.message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", probe.message, tc.want)
			}
		})
	}
}

func TestDSNForDatabaseReplacesOnlyTheName(t *testing.T) {
	got := parseDSN(dsnForDatabase("host=localhost port=5432 user=ada dbname=postgres sslmode=disable", "scratch"))
	if got["dbname"] != "scratch" {
		t.Errorf("dbname = %q", got["dbname"])
	}
	for key, want := range map[string]string{
		"host": "localhost", "port": "5432", "user": "ada", "sslmode": "disable",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// fatalRecorder stands in for *testing.T so a failure path can be asserted
// without failing the test that asserts it.
type fatalRecorder struct{ message string }

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	if f.message == "" {
		f.message = sprintf(format, args...)
	}
}

// The drop is registered the instant the database exists, before anything
// that can fail. Everything after CREATE DATABASE calls t.Fatalf on failure,
// and Fatalf calls runtime.Goexit, so a cleanup registered later would never
// run — a developer iterating on a broken migration would accumulate one
// scratch database per attempt. (Observed: ten, before this was fixed.)
//
// The failure path itself cannot be driven through New, because New takes a
// *testing.T and its Fatalf would fail whichever test called it. So this
// asserts the two halves that can be checked: the teardown really drops, and
// it is safe to call more than once, which is what makes registering it early
// harmless.
func TestTearDownDropsTheDatabaseAndIsIdempotent(t *testing.T) {
	skipWithoutPostgres(t)
	a := New(t, WithConfig(postgresConfig()), WithScratchDBNoMigrate())

	name := a.scratch.name
	adminDSN := a.scratch.adminDSN
	if !databaseExists(t, adminDSN, name) {
		t.Fatalf("%s was not created", name)
	}

	a.tearDownPostgres()
	if databaseExists(t, adminDSN, name) {
		t.Errorf("%s survived teardown", name)
	}

	// Called again by t.Cleanup at the end of the test; it must not fail or
	// panic on an already-dropped database.
	a.tearDownPostgres()
}

func databaseExists(t *testing.T, adminDSN, name string) bool {
	t.Helper()
	db, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// testAdminDSN is the server these tests create scratch databases on.
//
// It calls the builder the library itself uses rather than formatting a DSN
// beside it. A second copy would have to be kept in step, and the first
// version of it was already wrong in a way worth remembering: it wrote
// `password=` with no value for a server that needs no password, and lib/pq
// read the rest of the string as part of that empty value — so sslmode never
// arrived and the connection failed with "SSL is not enabled on the server",
// which is the exact error this whole change exists to stop producing.
func testAdminDSN(t *testing.T) string {
	t.Helper()
	return adminDSNFromConfig(t, postgresConfig())
}

// The connection block's options reach the scratch database.
//
// This is the regression the whole change is for. The old code took host,
// user, password and port from an environment DSN and left options alone, so
// a configuration saying sslmode=disable could be opened against a server
// reached with sslmode=require — and every test in the package died with
// "pq: SSL is not enabled on the server". Reading one source makes that
// unrepresentable, and this asserts the one source is the configuration.
func TestTheConnectionsOptionsSurviveIntoTheScratchDSN(t *testing.T) {
	cfg := postgresConfig()
	conn := cfg["sql"].(config.M)["connections"].(config.M)["postgres"].(config.M)
	conn["options"] = config.M{"sslmode": "disable", "connect_timeout": "7"}

	dsn := adminDSNFromConfig(t, cfg)
	for _, want := range []string{"sslmode=disable", "connect_timeout=7"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q is missing %q", dsn, want)
		}
	}
}

// An empty password is omitted rather than written as an empty value.
//
// `password=` followed by a space is read by lib/pq as a value that swallows
// what comes after it, so the options behind it never arrive. This cost an
// hour the first time, from a test helper that formatted its own DSN.
func TestAnEmptyPasswordIsLeftOutOfTheDSN(t *testing.T) {
	cfg := postgresConfig()
	conn := cfg["sql"].(config.M)["connections"].(config.M)["postgres"].(config.M)
	conn["password"] = ""

	dsn := adminDSNFromConfig(t, cfg)
	if strings.Contains(dsn, "password=") {
		t.Errorf("dsn %q writes an empty password", dsn)
	}
	if !strings.Contains(dsn, "sslmode=") {
		t.Errorf("dsn %q lost its options", dsn)
	}
}

// A non-Postgres application is told what is wrong, not handed a confusing
// connection error.
func TestANonPostgresConnectionIsRefusedClearly(t *testing.T) {
	cfg := postgresConfig()
	cfg["sql"].(config.M)["connections"].(config.M)["postgres"].(config.M)["driver"] = "mysql"

	probe := &fatalRecorder{}
	adminDSNFromConfig(probe, cfg)
	if !strings.Contains(probe.message, "implemented for postgres only") {
		t.Errorf("message = %q, want it to name the limitation", probe.message)
	}
}
