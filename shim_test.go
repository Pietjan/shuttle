package shuttle

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestShimReportsConnectionState. Datastar gives up permanently after ten
// failed retries, so without something watching, a page whose server went
// away just goes quiet and looks fine.
func TestShimReportsConnectionState(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)

	for _, want := range []string{
		"datastar-fetch",
		"datastar-patch-",
		"retries-failed",
		"shuttle-reconnecting",
		"shuttle-dead",
		"shuttleState",
		// Patch events are the proof the stream is alive; the heartbeat
		// keeps them arriving on an idle page.
		routePrefix + "/up",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("shim missing %q", want)
		}
	}
}

// TestHealthEndpointCostsNothing: a page whose session has died polls this
// until the server answers, so it must not mint a session per attempt.
func TestHealthEndpointCostsNothing(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for range 5 {
		resp, err := srv.Client().Get(srv.URL + routePrefix + "/up")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("probe: status %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("probe Cache-Control = %q, want no-store", got)
		}
	}

	if n := h.sessions.len(); n != 0 {
		t.Errorf("the health probe created %d sessions", n)
	}
}

// TestHeartbeatKeepsTheStreamWriting. A quiet connection is closed by
// proxies, and a write is the only way this end finds out the client has
// gone.
func TestHeartbeatKeepsTheStreamWriting(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Heartbeat = 20 * time.Millisecond
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// An empty signal patch: nothing changes on the client, but the bytes
	// travel.
	evt := stream.event(t)
	if !strings.Contains(evt, "datastar-patch-signals") {
		t.Errorf("heartbeat is not a signal patch: %q", evt)
	}
	if !strings.Contains(evt, "{}") {
		t.Errorf("heartbeat carries a payload: %q", evt)
	}

	// And it keeps going.
	if second := stream.event(t); !strings.Contains(second, "datastar-patch-signals") {
		t.Errorf("heartbeat stopped after one beat: %q", second)
	}
}

// TestHeartbeatCanBeTurnedOff, for a deployment that would rather not have
// the traffic.
func TestHeartbeatCanBeTurnedOff(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Heartbeat = -1
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	if !stream.silent(200 * time.Millisecond) {
		t.Error("a disabled heartbeat still wrote to the stream")
	}
}

// TestHeartbeatDefaults.
func TestHeartbeatDefaults(t *testing.T) {
	h := New(func() Component { return &counter{} })
	if got := h.heartbeat(); got != DefaultHeartbeat {
		t.Errorf("unset heartbeat = %v, want %v", got, DefaultHeartbeat)
	}

	h.Heartbeat = time.Second
	if got := h.heartbeat(); got != time.Second {
		t.Errorf("heartbeat = %v, want 1s", got)
	}
}

// TestShimIsInPageScripts, so a custom Shell can place it and knows what it
// loses by dropping it.
func TestShimIsInPageScripts(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()

	var got Page
	h.Shell = func(w io.Writer, p Page) error {
		got = p
		return DefaultShell(w, p)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	getPage(t, srv)

	if !strings.Contains(got.Scripts, "popstate") {
		t.Errorf("Page.Scripts has no history shim: %q", got.Scripts)
	}
	if !strings.Contains(got.Scripts, "datastar-fetch") {
		t.Errorf("Page.Scripts has no connection watcher: %q", got.Scripts)
	}
}

// TestStreamGreetsOnConnect. A client cannot tell a live stream from a
// stalled one until bytes arrive, and Datastar's own retry is silent - so a
// page that has just reconnected went on showing "connection lost" over a
// working connection until the next heartbeat, which is 25 seconds by
// default. Found by an e2e test that waited 20 seconds and then passed at 27.
//
// The greeting is unconditional: it is proof of life, not a heartbeat, so
// turning the heartbeat off does not turn it off.
func TestStreamGreetsOnConnect(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Heartbeat = -1
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)

	req, err := http.NewRequest(http.MethodGet, srv.URL+routePrefix+"/live/"+sid, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	stream := newSSEReader(resp.Body)
	t.Cleanup(stream.close)

	evt := stream.event(t)
	if !strings.Contains(evt, "datastar-patch-signals") {
		t.Errorf("a stream opened with %q, want an empty signal patch", evt)
	}
	if !strings.Contains(evt, "{}") {
		t.Errorf("the greeting carries a payload: %q", evt)
	}

	// And then it goes quiet, because the heartbeat is off.
	if !stream.silent(200 * time.Millisecond) {
		t.Error("the stream kept writing with the heartbeat disabled")
	}
}
