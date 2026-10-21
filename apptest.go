// Package apptest boots a real Lemmego application in a test and drives it
// over HTTP, without binding a port.
//
// The point is that the application under test is the application that ships.
// It boots through app.Handler, so providers run, application and HTTP
// middleware are installed, routes are mounted and the session wrapper is in
// place — the same sequence Run performs before it starts listening. A harness
// that reassembled those pieces itself would drift from production, and a test
// that passes against a drifted harness is worse than no test.
//
// A minimal test:
//
//	func TestPing(t *testing.T) {
//	    a := apptest.New(t,
//	        apptest.WithConfig(configs.All()),
//	        apptest.WithRoutes(bootstrap.LoadRoutes()...),
//	    )
//	    a.Get("/api/ping").Send().AssertStatus(200).AssertJSONPath("message", "pong")
//	}
package apptest

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/lemmego/api/app"
	"github.com/lemmego/api/config"
	"github.com/lemmego/auth"
	"github.com/lemmego/inertia"
)

// actingAsHeader carries the subject for Request.ActingAs.
//
// It is a header rather than a configure-time option because authentication is
// a property of a request, not of an application, and one App serves many
// requests in a test. The middleware that reads it is installed only by this
// package, so it cannot exist in a production stack.
const actingAsHeader = "X-Apptest-Acting-As"

// App is a booted application, served in-process.
type App struct {
	t       *testing.T
	engine  app.AppEngine
	handler http.Handler
	jar     *cookiejar.Jar
	baseURL *url.URL

	ctx                  context.Context
	cachedInertiaVersion string
	inertiaVersionKnown  bool
	skipCSRF             bool
	strictSSR            bool
	serverOnce           sync.Once        // guards server, so Server is callable more than once
	server               *httptest.Server // non-nil once Server has been called
	scratch              *scratchDB       // non-nil when WithPostgres created a database
	cachePrefix          string           // the namespace this App's cache entries live under
}

type options struct {
	config          config.M
	providers       []app.Provider
	routes          []app.RouteCallback
	middlewares     []app.Handler
	httpMiddlewares []app.HTTPMiddleware
	errMap          app.ErrMap
	postgres        bool
	migrate         bool
	workingDir      string
	keepWorkingDir  bool
	sharedCache     bool
	skipCSRF        bool
	strictSSR       bool
}

// Option configures the application a test boots.
type Option func(*options)

// WithConfig sets the configuration map, as cmd/app/main.go does with
// config.GetAll(). Without it the application gets only the minimum needed to
// boot, which is enough for routing tests and nothing else.
func WithConfig(c config.M) Option { return func(o *options) { o.config = c } }

// WithProviders registers providers, in order. Pass bootstrap.LoadProviders()
// to test against the real stack, or a subset to isolate one concern.
func WithProviders(p ...app.Provider) Option {
	return func(o *options) { o.providers = append(o.providers, p...) }
}

// WithRoutes registers route callbacks. Pass bootstrap.LoadRoutes()...
func WithRoutes(cb ...app.RouteCallback) Option {
	return func(o *options) { o.routes = append(o.routes, cb...) }
}

// WithMiddlewares registers application middleware.
//
// Note that a test which posts a form through the real middleware stack will
// meet VerifyCSRF, exactly as a browser does. Read the token from the page
// rather than disabling the middleware: a test that bypasses CSRF cannot tell
// you whether your form works.
func WithMiddlewares(h ...app.Handler) Option {
	return func(o *options) { o.middlewares = append(o.middlewares, h...) }
}

// WithHTTPMiddlewares registers net/http-level middleware.
func WithHTTPMiddlewares(m ...app.HTTPMiddleware) Option {
	return func(o *options) { o.httpMiddlewares = append(o.httpMiddlewares, m...) }
}

// WithErrMap registers the application's error map, so a test sees the status
// codes and pages the application really returns for its own error types.
func WithErrMap(m app.ErrMap) Option { return func(o *options) { o.errMap = m } }

// WithStrictSSR makes a failed server-side render a 500 in this test, as it
// is outside production.
//
// Off by default for a test boot, and that default is the interesting part.
// A test has no Node renderer under its control: whether one answers depends
// on whether somebody happens to be running `npm run dev` on this machine,
// and in CI nothing is. Strict mode would therefore turn every page
// assertion into a 500 on one machine and a 200 on another, which is a test
// suite that reports where it ran rather than whether the code works.
//
// So a test boot is lenient and a test that wants to assert on rendering says
// so. Pair it with Response.AssertServerRendered, and be aware that it needs
// a renderer actually running.
func WithStrictSSR() Option { return func(o *options) { o.strictSSR = true } }

// New boots an application and returns it ready to receive requests.
//
// It fails the test rather than returning an error: a harness that cannot boot
// has nothing useful to say, and threading an error through every call site
// buys nothing.
func New(t *testing.T, opts ...Option) *App {
	t.Helper()

	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	cfg := o.config
	if cfg == nil {
		cfg = config.M{}
	}
	// app.Handler panics without these two, and a test that forgot to pass a
	// config should get a working application rather than a panic.
	if _, ok := cfg["app"]; !ok {
		cfg["app"] = config.M{"name": "apptest", "env": "test", "port": 0}
	}

	a := &App{t: t, baseURL: &url.URL{Scheme: "http", Host: "apptest.local"}, skipCSRF: o.skipCSRF}

	// Before anything reads a project-relative path.
	switch {
	case o.keepWorkingDir:
	case o.workingDir != "":
		t.Chdir(o.workingDir)
	default:
		chdirToProjectRoot(t)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("apptest: cookie jar: %v", err)
	}
	a.jar = jar

	if !o.sharedCache {
		a.isolateCache(o)
	}

	if o.postgres {
		a.setUpPostgres(o)
	}

	// The With* methods mutate the engine and return it as a Bootstrapper, so
	// the returns are discarded and the AppEngine reference is kept — it is
	// the wider interface, and the test needs it for App() and Handler().
	engine := app.Configure()
	engine.WithConfig(cfg)
	if len(o.providers) > 0 {
		engine.WithProviders(o.providers)
	}
	if len(o.routes) > 0 {
		engine.WithRoutes(o.routes)
	}
	if len(o.httpMiddlewares) > 0 {
		engine.WithHTTPMiddlewares(o.httpMiddlewares)
	}
	// The acting-as middleware is prepended, so it establishes the subject
	// before auth.Protected (or anything else) asks who is making the request.
	engine.WithMiddlewares(append([]app.Handler{actingAsMiddleware()}, o.middlewares...))
	if o.errMap != nil {
		engine.WithErrMap(o.errMap)
	}

	a.engine = engine
	a.handler = engine.Handler()

	// After the providers have run, because inertia.Provider decides
	// strictness from app.env and a test boot overrides that decision. The
	// previous value is restored so one lenient test does not quietly
	// relax a later strict one.
	previous := inertia.StrictSSR()
	inertia.SetStrictSSR(o.strictSSR)
	t.Cleanup(func() { inertia.SetStrictSSR(previous) })

	return a
}

// actingAsMiddleware turns the acting-as header into a verified subject.
//
// It calls auth.SetSubject rather than inventing a user, so the application's
// own UserLoader runs and the handler under test receives the same concrete
// type it would in production. A stub user would make every typed-accessor
// test vacuous.
func actingAsMiddleware() app.Handler {
	return func(c app.Context) error {
		if id := c.Header(actingAsHeader); id != "" {
			auth.SetSubject(c, id)
		}
		return c.Next()
	}
}

// App returns the booted application, for a test that needs to reach into the
// container.
func (a *App) App() app.App { return a.engine }

// Handler returns the HTTP handler under test, for a test that wants to drive
// it directly rather than through the request builder.
func (a *App) Handler() http.Handler { return a.handler }

// Get starts a GET request.
func (a *App) Get(path string) *Request { return a.request(http.MethodGet, path) }

// Post starts a POST request.
func (a *App) Post(path string) *Request { return a.request(http.MethodPost, path) }

// Put starts a PUT request.
func (a *App) Put(path string) *Request { return a.request(http.MethodPut, path) }

// Patch starts a PATCH request.
func (a *App) Patch(path string) *Request { return a.request(http.MethodPatch, path) }

// Delete starts a DELETE request.
func (a *App) Delete(path string) *Request { return a.request(http.MethodDelete, path) }

// Ctx returns a context for the test's own database work — seeding a fixture,
// asserting against a row afterwards.
//
// It is the test's context, not a request's: a request gets its own from the
// handler. This one is cancelled when the test ends, so a query left running
// by a failed assertion does not outlive it.
func (a *App) Ctx() context.Context {
	if a.ctx == nil {
		ctx, cancel := context.WithCancel(context.Background())
		a.t.Cleanup(cancel)
		a.ctx = ctx
	}
	return a.ctx
}
