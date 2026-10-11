package apptest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
)

// dataPagePattern finds the page payload Inertia embeds in a document.
var dataPagePattern = regexp.MustCompile(`(?s)<script[^>]*data-page[^>]*>(.*?)</script>`)

// inertiaVersion returns the application's current asset version, discovering
// it once and caching it for the life of the App.
//
// It is discovered rather than configured because only the application knows
// it: the version is a hash of the Vite manifest, computed at boot from a file
// that may not even exist yet. Asking every test to pass it would be asking
// them to recompute something the application already did.
//
// Discovery is a plain GET of "/", whose rendered document carries the
// version inside the data-page payload. That request is a side effect, which
// is why it happens once and only for a test that actually makes an Inertia
// request.
//
// An application with no asset version — no manifest, or versioning switched
// off — yields "", which is exactly what such an application expects a client
// to send.
func (a *App) inertiaVersion() string {
	if a.inertiaVersionKnown {
		return a.cachedInertiaVersion
	}
	a.inertiaVersionKnown = true

	recorder := httptest.NewRecorder()
	probe := httptest.NewRequest(http.MethodGet, a.baseURL.String()+"/", nil)
	a.handler.ServeHTTP(recorder, probe)

	match := dataPagePattern.FindSubmatch(recorder.Body.Bytes())
	if match == nil {
		return ""
	}
	var page struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(match[1], &page); err != nil {
		return ""
	}
	a.cachedInertiaVersion = page.Version
	return page.Version
}
