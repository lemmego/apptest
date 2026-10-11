package apptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Response is a completed response. Every assertion returns the Response, so
// they chain, and every one marks the test as a helper so a failure points at
// the test's line rather than this file's.
type Response struct {
	t *testing.T
	*http.Response
	Body    []byte
	request *http.Request
}

// Text returns the body as a string.
func (r *Response) Text() string { return string(r.Body) }

// AssertStatus fails unless the status matches.
func (r *Response) AssertStatus(want int) *Response {
	r.t.Helper()
	if r.StatusCode != want {
		r.t.Errorf("%s %s: status = %d, want %d\n%s",
			r.request.Method, r.request.URL.Path, r.StatusCode, want, r.excerpt())
	}
	return r
}

// AssertRedirect fails unless the response redirects to location.
func (r *Response) AssertRedirect(location string) *Response {
	r.t.Helper()
	if r.StatusCode < 300 || r.StatusCode > 399 {
		r.t.Errorf("%s %s: status = %d, want a redirect\n%s",
			r.request.Method, r.request.URL.Path, r.StatusCode, r.excerpt())
		return r
	}
	if got := r.Header.Get("Location"); got != location {
		r.t.Errorf("Location = %q, want %q", got, location)
	}
	return r
}

// AssertHeader fails unless the response header matches.
func (r *Response) AssertHeader(key, want string) *Response {
	r.t.Helper()
	if got := r.Header.Get(key); got != want {
		r.t.Errorf("header %s = %q, want %q", key, got, want)
	}
	return r
}

// AssertCookie fails unless the response sets a cookie of this name.
func (r *Response) AssertCookie(name string) *Response {
	r.t.Helper()
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			return r
		}
	}
	r.t.Errorf("no cookie named %q was set", name)
	return r
}

// AssertCookieExpired fails unless the response expires a cookie of this
// name. This is how you prove a logout actually revoked something rather
// than just answering 200.
func (r *Response) AssertCookieExpired(name string) *Response {
	r.t.Helper()
	for _, cookie := range r.Cookies() {
		if cookie.Name == name && (cookie.MaxAge < 0 || cookie.Value == "") {
			return r
		}
	}
	r.t.Errorf("cookie %q was not expired", name)
	return r
}

// AssertSee fails unless the body contains substr.
func (r *Response) AssertSee(substr string) *Response {
	r.t.Helper()
	if !strings.Contains(r.Text(), substr) {
		r.t.Errorf("body does not contain %q\n%s", substr, r.excerpt())
	}
	return r
}

// AssertDontSee fails if the body contains substr. Reach for it when the
// absence is the point — a password hash, another user's email, a draft.
func (r *Response) AssertDontSee(substr string) *Response {
	r.t.Helper()
	if strings.Contains(r.Text(), substr) {
		r.t.Errorf("body contains %q and should not\n%s", substr, r.excerpt())
	}
	return r
}

var headPattern = regexp.MustCompile(`(?is)<head[^>]*>(.*?)</head>`)

// AssertHeadContains fails unless every substring appears inside <head>.
//
// This is the SEO regression net. Page metadata travels through several layers
// — handler, shared props, root template, and possibly a server-side render —
// and when it breaks, the page still answers 200 and still looks right to a
// person. Only a crawler notices. Asserting on <head> specifically, rather
// than the whole body, keeps the assertion honest: a title that happens to
// appear in the page copy is not a title tag.
func (r *Response) AssertHeadContains(substrs ...string) *Response {
	r.t.Helper()
	match := headPattern.FindSubmatch(r.Body)
	if match == nil {
		r.t.Errorf("response has no <head> (status %d)\n%s", r.StatusCode, r.excerpt())
		return r
	}
	head := string(match[1])
	for _, want := range substrs {
		if !strings.Contains(head, want) {
			r.t.Errorf("<head> does not contain %q\nhead was:\n%s", want, head)
		}
	}
	return r
}

// AssertServerRendered fails unless the document was rendered server-side.
//
// A 200 proves nothing about server-side rendering: Inertia falls back to the
// client-rendered container on any SSR error, so a broken or absent SSR
// process still produces a perfectly healthy-looking response with an empty
// root element. If search traffic matters, this is the assertion that catches
// it — and it is the only one that does.
func (r *Response) AssertServerRendered() *Response {
	r.t.Helper()
	if !strings.Contains(r.Text(), `data-server-rendered="true"`) {
		r.t.Errorf("document was not server-rendered — Inertia fell back to the client-rendered container, which answers 200 either way\n%s", r.excerpt())
	}
	return r
}

// AssertJSONPath fails unless the value at a dotted path equals want.
//
// Paths index into objects by key and arrays by number: "user.email",
// "items.0.id". Numbers compare numerically, so 1 and 1.0 match, which keeps
// a test from caring that JSON has one number type.
func (r *Response) AssertJSONPath(path string, want any) *Response {
	r.t.Helper()

	var decoded any
	if err := json.Unmarshal(r.Body, &decoded); err != nil {
		r.t.Errorf("response is not JSON: %v\n%s", err, r.excerpt())
		return r
	}

	got, err := walkJSON(decoded, path)
	if err != nil {
		r.t.Errorf("%v\nbody:\n%s", err, r.excerpt())
		return r
	}
	if !jsonEqual(got, want) {
		r.t.Errorf("%s = %#v, want %#v", path, got, want)
	}
	return r
}

// AssertJSONPathAbsent fails if the path resolves. Use it to prove a field is
// not being serialised — a password hash is the obvious one.
func (r *Response) AssertJSONPathAbsent(path string) *Response {
	r.t.Helper()
	var decoded any
	if err := json.Unmarshal(r.Body, &decoded); err != nil {
		r.t.Errorf("response is not JSON: %v", err)
		return r
	}
	if got, err := walkJSON(decoded, path); err == nil {
		r.t.Errorf("%s is present with value %#v and should be absent", path, got)
	}
	return r
}

// JSONValue returns the value at a dotted path, for an assertion this package
// does not provide.
func (r *Response) JSONValue(path string) any {
	r.t.Helper()
	var decoded any
	if err := json.Unmarshal(r.Body, &decoded); err != nil {
		r.t.Fatalf("response is not JSON: %v", err)
	}
	got, err := walkJSON(decoded, path)
	if err != nil {
		r.t.Fatalf("%v", err)
	}
	return got
}

func walkJSON(node any, path string) (any, error) {
	current := node
	for _, segment := range strings.Split(path, ".") {
		switch typed := current.(type) {
		case map[string]any:
			value, ok := typed[segment]
			if !ok {
				return nil, fmt.Errorf("no value at %q (missing %q)", path, segment)
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil {
				return nil, fmt.Errorf("no value at %q (%q is not an array index)", path, segment)
			}
			if index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("no value at %q (index %d out of range, length %d)", path, index, len(typed))
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("no value at %q (%q is not traversable)", path, segment)
		}
	}
	return current, nil
}

// jsonEqual compares numbers numerically, so a test can write 1 where JSON
// decoded 1.0 without either being wrong.
func jsonEqual(got, want any) bool {
	gotNum, gotOK := asFloat(got)
	wantNum, wantOK := asFloat(want)
	if gotOK && wantOK {
		return gotNum == wantNum
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// excerpt bounds what a failure prints. A full HTML page in a test log buries
// the assertion that failed.
func (r *Response) excerpt() string {
	const limit = 1200
	body := r.Text()
	if len(body) > limit {
		return body[:limit] + "\n… (" + strconv.Itoa(len(body)-limit) + " more bytes)"
	}
	return body
}
