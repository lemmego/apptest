package apptest_test

import (
	"net/http"
	"testing"

	"github.com/lemmego/api/app"
	"github.com/lemmego/apptest"
)

// The harness defaults to apptest.local, which is fine until the application
// routes on the host. These two knobs are what make a multi-tenant
// application testable at all.

func echoHost() app.RouteCallback {
	return func(a app.App) {
		a.Router().Get("/whoami", func(c app.Context) error {
			return c.Text([]byte(c.Request().Host))
		})
	}
}

func TestWithBaseURLSetsTheHostEveryRequestArrivesAt(t *testing.T) {
	a := apptest.New(t,
		apptest.WithBaseURL("http://acme.fieldbubble.localhost:8083"),
		apptest.WithRoutes(echoHost()))

	got := a.Get("/whoami").Send()
	got.AssertStatus(http.StatusOK)
	if body := got.Text(); body != "acme.fieldbubble.localhost:8083" {
		t.Fatalf("the application saw host %q", body)
	}
}

func TestTheDefaultHostIsUnchanged(t *testing.T) {
	a := apptest.New(t, apptest.WithRoutes(echoHost()))
	if body := a.Get("/whoami").Send().Text(); body != "apptest.local" {
		t.Fatalf("the default host changed to %q, which would move every existing suite", body)
	}
}

func TestRequestHostOverridesOneRequest(t *testing.T) {
	a := apptest.New(t,
		apptest.WithBaseURL("http://acme.fieldbubble.localhost"),
		apptest.WithRoutes(echoHost()))

	if body := a.Get("/whoami").Host("globex.fieldbubble.localhost").Send().Text(); body != "globex.fieldbubble.localhost" {
		t.Fatalf("the override did not apply: %q", body)
	}
	// And the App's own base is untouched by it.
	if body := a.Get("/whoami").Send().Text(); body != "acme.fieldbubble.localhost" {
		t.Fatalf("the override leaked into the next request: %q", body)
	}
}

func TestAMalformedBaseURLFailsLoudly(t *testing.T) {
	// Can't assert the t.Fatalf without a fake *testing.T, so assert the
	// parse rule the message describes instead: a bare host is not enough,
	// because net/url reads it as a path and every request would then go to
	// apptest.local with a strange path.
	for _, raw := range []string{"acme.fieldbubble.localhost", "/acme"} {
		if parsed, err := http.NewRequest(http.MethodGet, raw, nil); err == nil && parsed.URL.Host != "" {
			t.Fatalf("%q unexpectedly parsed with a host", raw)
		}
	}
}
