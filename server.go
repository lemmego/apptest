package apptest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
)

// ActingAsHeader is the header Request.ActingAs sets, exported so a test that
// drives the application over a real connection can authenticate the same way.
//
// A stream and a WebSocket upgrade cannot go through the request builder (see
// Server), so without this a test of either would have no way to say who is
// making the request short of performing a real sign-in.
const ActingAsHeader = actingAsHeader

// Server starts a real HTTP server over the application under test, and
// returns it.
//
// Almost every test should use the request builder instead. This exists for
// the two kinds of request a httptest.ResponseRecorder cannot represent:
//
//   - A stream. Send invokes the handler and returns when the handler returns,
//     so an endpoint that holds its response open — server-sent events — never
//     returns at all, and the test hangs rather than failing.
//   - An upgrade. A recorder does not implement http.Hijacker, so a WebSocket
//     handshake fails on the hijack rather than on anything the test meant to
//     exercise.
//
// The server is closed when the test ends. Calling Server more than once on
// one App returns the same server, so a helper may ask for it without
// coordinating with the test that already did.
func (a *App) Server() *httptest.Server {
	a.t.Helper()

	a.serverOnce.Do(func() {
		server := httptest.NewServer(a.handler)
		a.t.Cleanup(server.Close)
		a.server = server
	})
	return a.server
}

// Client returns an HTTP client that shares this App's cookies.
//
// The cookie jar is the App's own, so a sign-in performed through the request
// builder authenticates a connection opened through this client, and a cookie
// the server sets on this client is visible to a later builder request. That
// matters more here than it looks: an EventSource and a browser WebSocket
// cannot send an Authorization header, so cookie authentication is the only
// mechanism those endpoints have in production, and a harness that could not
// carry a cookie across the two transports would be testing a different
// mechanism than the one that ships.
//
// Redirects are not followed. A stream endpoint that answers 302 is a failure
// the test should see, not something to chase.
func (a *App) Client() *http.Client {
	a.t.Helper()

	return &http.Client{
		Jar: &bridgedJar{jar: a.jar, base: a.baseURL},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// bridgedJar maps every host onto the App's base URL.
//
// The App's jar is keyed by URL, and a httptest.Server listens on
// 127.0.0.1 with a port the kernel chose, so cookies stored while driving the
// handler directly would not be offered to the server and vice versa. One
// application in one test is one cookie identity regardless of how the bytes
// reached it, so the mapping is the honest behaviour rather than a shortcut.
type bridgedJar struct {
	jar  http.CookieJar
	base *url.URL
}

func (b *bridgedJar) Cookies(*url.URL) []*http.Cookie {
	return b.jar.Cookies(b.base)
}

func (b *bridgedJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	b.jar.SetCookies(b.base, cookies)
}
