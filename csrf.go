package apptest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
)

// csrfCookie is the cookie the framework mirrors the session token into, and
// csrfHeader is where a client presents it. Both match what
// @inertiajs/core sends by default, which is why an Inertia application needs
// no CSRF wiring of its own.
const (
	csrfCookie = "XSRF-TOKEN"
	csrfHeader = "X-XSRF-TOKEN"
)

// attachCSRF presents the CSRF token on an unsafe request, the way a browser
// client does.
//
// This is not a bypass. The token is session-backed: the middleware compares
// what the request presents against what the session holds, so a request can
// only satisfy it by having first obtained a session — which is exactly the
// property CSRF protection exists to assert. A test that posts a form still
// exercises the real middleware, and a handler that forgot to be behind it
// would still be caught.
//
// Where the token does not already exist, it is obtained by a GET — because a
// browser cannot post a form it never loaded either. Use WithoutCSRFToken to
// suppress all of this and assert on the rejection itself.
func (r *Request) attachCSRF(target *url.URL) {
	if r.app.skipCSRF || r.header.Get(csrfHeader) != "" {
		return
	}
	switch r.method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return
	}

	token := r.app.csrfToken(target)
	if token != "" {
		r.header.Set(csrfHeader, token)
	}
}

// csrfToken returns the token from the cookie jar, establishing a session
// first if there is none.
func (a *App) csrfToken(target *url.URL) string {
	if token := a.cookieValue(target, csrfCookie); token != "" {
		return token
	}

	// No session yet. A GET of "/" issues one, exactly as loading a page in a
	// browser would, and the jar keeps the cookies.
	root := a.baseURL.ResolveReference(&url.URL{Path: "/"})
	recorder := httptest.NewRecorder()
	probe := httptest.NewRequest(http.MethodGet, root.String(), nil)
	for _, cookie := range a.jar.Cookies(root) {
		probe.AddCookie(cookie)
	}
	a.handler.ServeHTTP(recorder, probe)
	if cookies := recorder.Result().Cookies(); len(cookies) > 0 {
		a.jar.SetCookies(root, cookies)
	}

	return a.cookieValue(target, csrfCookie)
}

func (a *App) cookieValue(target *url.URL, name string) string {
	for _, cookie := range a.jar.Cookies(target) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

// WithoutCSRFToken stops the harness presenting a CSRF token, so an unsafe
// request arrives without one.
//
// Use it to assert that the protection is actually in place — a test that only
// ever sends a valid token cannot tell a guarded route from an unguarded one.
func WithoutCSRFToken() Option {
	return func(o *options) { o.skipCSRF = true }
}
