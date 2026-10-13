package apptest

import (
	"bufio"
	"fmt"
	"net/http"
	"testing"

	"github.com/lemmego/api/app"
	"github.com/lemmego/auth"
)

// These test the two cases Server exists for. Each one, driven through the
// request builder instead, fails in a way that tells the reader nothing: the
// stream hangs until the test binary's timeout, and the hijack reports that
// the ResponseWriter does not implement an interface.

func streamRoutes(a app.App) {
	a.Router().Get("/stream", func(c app.Context) error {
		w := c.ResponseWriter()
		flusher, ok := w.(http.Flusher)
		if !ok {
			return fmt.Errorf("the response writer does not flush")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
		}
		// Held open, as a real stream is, until the client goes away. Without
		// this the handler returns and the test would pass against a recorder
		// too, proving nothing.
		<-c.Request().Context().Done()
		return nil
	})
}

func TestServerCarriesAStreamTheRequestBuilderCannot(t *testing.T) {
	a := New(t, WithRoutes(streamRoutes))
	server := a.Server()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	response, err := a.Client().Do(request)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("got content type %q, want text/event-stream", got)
	}

	// Read the events that have already been flushed. The response arriving at
	// all is the point: the handler has not returned and will not until this
	// body is closed.
	reader := bufio.NewReader(response.Body)
	for i := 0; i < 3; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event %d: %v", i, err)
		}
		if want := fmt.Sprintf("data: %d\n", i); line != want {
			t.Fatalf("got %q, want %q", line, want)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("reading the blank line after event %d: %v", i, err)
		}
	}
}

func TestServerAllowsAHijack(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Get("/hijack", func(c app.Context) error {
			hijacker, ok := c.ResponseWriter().(http.Hijacker)
			if !ok {
				return fmt.Errorf("the response writer cannot be hijacked")
			}
			conn, buffered, err := hijacker.Hijack()
			if err != nil {
				return err
			}
			defer conn.Close()
			// Spoken by hand, because after a hijack there is no net/http
			// response to write. This is what a WebSocket handshake does.
			fmt.Fprint(buffered, "HTTP/1.1 101 Switching Protocols\r\n\r\n")
			return buffered.Flush()
		})
	}))
	server := a.Server()

	response, err := a.Client().Get(server.URL + "/hijack")
	if err != nil {
		t.Fatalf("requesting the hijacked route: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("got status %d, want 101 — the handler could not hijack",
			response.StatusCode)
	}
}

func TestServerSharesTheAppsCookies(t *testing.T) {
	a := New(t, WithRoutes(func(a app.App) {
		a.Router().Get("/issue", func(c app.Context) error {
			c.SetCookie(&http.Cookie{Name: "visit", Value: "first", Path: "/"})
			return c.JSON(app.M{"ok": true})
		})
		a.Router().Get("/read", func(c app.Context) error {
			cookie := c.Cookie("visit")
			if cookie == nil {
				return c.JSON(app.M{"visit": ""})
			}
			return c.JSON(app.M{"visit": cookie.Value})
		})
	}))

	// Issued through the request builder, which stores it under the App's own
	// base URL.
	a.Get("/issue").Send().AssertStatus(http.StatusOK)

	// Read back over a real connection, whose host is 127.0.0.1 and a port the
	// kernel chose. Without the jar bridge this is empty, and a test of a
	// cookie-authenticated stream would silently be testing an anonymous one.
	server := a.Server()
	response, err := a.Client().Get(server.URL + "/read")
	if err != nil {
		t.Fatalf("reading the cookie back: %v", err)
	}
	defer response.Body.Close()

	buffer := make([]byte, 256)
	n, _ := response.Body.Read(buffer)
	if body := string(buffer[:n]); body != `{"visit":"first"}` {
		t.Fatalf("got %s, want the cookie issued through the request builder", body)
	}
}

func TestServerIsTheSameServerEachTime(t *testing.T) {
	a := New(t, WithRoutes(pingRoutes))
	if a.Server().URL != a.Server().URL {
		t.Fatal("Server returned two different servers, so a helper and a test " +
			"asking for one would drive two applications")
	}
}

func TestActingAsHeaderAuthenticatesOverARealConnection(t *testing.T) {
	a := New(t,
		WithRoutes(func(a app.App) {
			a.Router().Get("/who", func(c app.Context) error {
				id, _ := auth.UserID(c)
				return c.JSON(app.M{"id": id})
			})
		}),
	)
	server := a.Server()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/who", nil)
	request.Header.Set(ActingAsHeader, "42")
	response, err := a.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting as somebody: %v", err)
	}
	defer response.Body.Close()

	buffer := make([]byte, 128)
	n, _ := response.Body.Read(buffer)
	if body := string(buffer[:n]); body != `{"id":"42"}` {
		t.Fatalf("got %s, want the acting-as subject — a stream test has no "+
			"other way to say who is connecting", body)
	}
}
