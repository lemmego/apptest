package apptest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lemmego/api/app"
	"github.com/lemmego/api/config"
	"github.com/lemmego/auth"
)

// A harness has to be tested against the framework it harnesses, or it tests
// only itself. These boot real applications.

func pingRoutes(a app.App) {
	a.Router().Get("/api/ping", func(c app.Context) error {
		return c.JSON(app.M{"message": "pong"})
	})
}

func TestSendsRequestsAndAssertsJSON(t *testing.T) {
	a := New(t, WithRoutes(pingRoutes))

	a.Get("/api/ping").Send().
		AssertStatus(http.StatusOK).
		AssertJSONPath("message", "pong")
}

func TestJSONPathTraversesObjectsAndArrays(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Get("/nested", func(c app.Context) error {
			return c.JSON(app.M{
				"user":  app.M{"id": 7, "email": "ada@example.com"},
				"items": []app.M{{"id": 1}, {"id": 2}},
			})
		})
	}))

	res := a.Get("/nested").Send().AssertStatus(http.StatusOK)
	res.AssertJSONPath("user.email", "ada@example.com")
	// 7 decodes as float64; the assertion compares numerically so a test can
	// write the integer it means.
	res.AssertJSONPath("user.id", 7)
	res.AssertJSONPath("items.1.id", 2)
	res.AssertJSONPathAbsent("user.password")
}

// The request body and its Content-Type have to arrive together, or a handler
// that binds JSON sees an empty struct and the test passes for the wrong
// reason.
func TestPostsJSONBodies(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Post("/echo", func(c app.Context) error {
			var body struct {
				Name string `json:"name"`
			}
			if err := c.DecodeJSON(&body); err != nil {
				return err
			}
			return c.JSON(app.M{"got": body.Name, "type": c.Header("Content-Type")})
		})
	}))

	a.Post("/echo").JSON(map[string]any{"name": "Ada"}).Send().
		AssertStatus(http.StatusOK).
		AssertJSONPath("got", "Ada").
		AssertJSONPath("type", "application/json")
}

func TestPostsForms(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Post("/form", func(c app.Context) error {
			return c.JSON(app.M{"name": c.Request().FormValue("name")})
		})
	}))

	a.Post("/form").Form(map[string]string{"name": "Grace"}).Send().
		AssertStatus(http.StatusOK).
		AssertJSONPath("name", "Grace")
}

// ActingAs must go through the application's own UserLoader, so a handler
// receives the concrete type it would in production. A stubbed user would
// make every typed-accessor assertion vacuous.
func TestActingAsResolvesThroughTheApplicationsLoader(t *testing.T) {
	type user struct {
		ID    string
		Email string
	}

	loaded := 0
	a := New(t,
		WithProviders(&auth.Provider{Opts: &auth.Opts{
			DisableSession: true,
			JwtSecret:      "test-secret",
			UserLoader: func(c app.Context, id string) (any, error) {
				loaded++
				if id != "42" {
					return nil, auth.ErrUserNotFound
				}
				return &user{ID: id, Email: "ada@example.com"}, nil
			},
		}}),
		WithRoutes(func(a app.App) {
			a.Router().Get("/me", auth.Protected, func(c app.Context) error {
				u, ok := auth.UserAs[*user](c)
				if !ok {
					return c.JSON(app.M{"typed": false})
				}
				return c.JSON(app.M{"typed": true, "email": u.Email})
			})
		}),
	)

	a.Get("/me").ActingAs("42").Send().
		AssertStatus(http.StatusOK).
		AssertJSONPath("typed", true).
		AssertJSONPath("email", "ada@example.com")

	if loaded != 1 {
		t.Errorf("the application's UserLoader ran %d times, want exactly 1", loaded)
	}
}

// Without ActingAs the request is anonymous, so a guarded route must refuse
// it. If this ever passes, the harness is authenticating by accident.
func TestRequestsAreAnonymousByDefault(t *testing.T) {
	a := New(t,
		WithProviders(&auth.Provider{Opts: &auth.Opts{
			DisableSession: true,
			JwtSecret:      "test-secret",
			UserLoader: func(c app.Context, id string) (any, error) {
				return nil, auth.ErrUserNotFound
			},
		}}),
		WithRoutes(func(a app.App) {
			a.Router().Get("/guarded", auth.Protected, func(c app.Context) error {
				return c.JSON(app.M{"ok": true})
			})
		}),
	)

	a.Get("/guarded").Send().AssertStatus(http.StatusUnauthorized)
}

// An id the loader does not recognise must be refused, not admitted — the
// harness establishes a subject, it does not vouch for one.
func TestActingAsAnUnknownUserIsRefused(t *testing.T) {
	a := New(t,
		WithProviders(&auth.Provider{Opts: &auth.Opts{
			DisableSession: true,
			JwtSecret:      "test-secret",
			UserLoader: func(c app.Context, id string) (any, error) {
				return nil, auth.ErrUserNotFound
			},
		}}),
		WithRoutes(func(a app.App) {
			a.Router().Get("/guarded", auth.Protected, func(c app.Context) error {
				return c.JSON(app.M{"ok": true})
			})
		}),
	)

	a.Get("/guarded").ActingAs("999").Send().AssertStatus(http.StatusUnauthorized)
}

// Cookies have to persist across requests on one App, or a login cannot be
// followed by a page load and a CSRF token cannot survive to the POST that
// needs it.
func TestCookiesPersistAcrossRequests(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Get("/set", func(c app.Context) error {
			c.SetCookie(&http.Cookie{Name: "probe", Value: "carried", Path: "/"})
			return c.JSON(app.M{"ok": true})
		})
		a.Router().Get("/read", func(c app.Context) error {
			if cookie := c.Cookie("probe"); cookie != nil {
				return c.JSON(app.M{"value": cookie.Value})
			}
			return c.JSON(app.M{"value": ""})
		})
	}))

	a.Get("/set").Send().AssertStatus(http.StatusOK).AssertCookie("probe")
	a.Get("/read").Send().AssertJSONPath("value", "carried")
}

func TestAssertHeadContainsLooksOnlyInHead(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Get("/page", func(c app.Context) error {
			return c.HTML([]byte(`<html><head><title>Ada Lovelace</title></head><body>decoy canonical</body></html>`))
		})
	}))

	res := a.Get("/page").Send().AssertStatus(http.StatusOK)
	res.AssertHeadContains("<title>Ada Lovelace</title>")

	// A string present only in the body must not satisfy a head assertion —
	// otherwise the SEO net catches nothing. Checked by reaching past the
	// assertion to the extraction it relies on, so this test does not have to
	// fail in order to prove a failure.
	res.AssertSee("decoy canonical")
	head := headPattern.FindSubmatch(res.Body)
	if head == nil {
		t.Fatal("no <head> extracted")
	}
	if strings.Contains(string(head[1]), "decoy canonical") {
		t.Error("head extraction leaked body content, so AssertHeadContains would pass on body text")
	}
}

func TestQueryStringsReachTheHandler(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Get("/search", func(c app.Context) error {
			return c.JSON(app.M{"q": c.Request().URL.Query().Get("q")})
		})
	}))

	a.Get("/search?q=indie+hackers").Send().AssertJSONPath("q", "indie hackers")
}

// The configuration a test passes must reach the application, or every
// config-dependent provider behaves differently under test.
func TestConfigReachesTheApplication(t *testing.T) {
	a := New(t,
		WithConfig(config.M{
			"app":    config.M{"name": "productshore", "env": "test", "port": 0},
			"custom": config.M{"answer": 42},
		}),
		WithRoutes(func(a app.App) {
			a.Router().Get("/cfg", func(c app.Context) error {
				return c.JSON(app.M{
					"name":   c.App().Config().Get("app.name"),
					"answer": c.App().Config().Get("custom.answer"),
				})
			})
		}),
	)

	a.Get("/cfg").Send().
		AssertJSONPath("name", "productshore").
		AssertJSONPath("answer", 42)
}
