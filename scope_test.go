package shuttle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/input"
)

// search is the shape signal namespacing exists for: the query lives on the
// client so typing does not need a round trip, the results live on the
// server.
type search struct {
	Base
	Ran  int
	Seen map[string]any
}

func (s *search) Signals() map[string]any {
	return map[string]any{"query": "", "open": false}
}

func (s *search) Render(ctx context.Context) templ.Component {
	box := input.New(
		ID(ctx, input.ID, "query"),
		On(input.Attr, "input", Ref(ctx, "query")+" = evt.target.value"),
	)
	run := button.New(
		OnClick(ctx, button.Attr, func(actx context.Context) error {
			s.Ran++
			s.Seen = SignalValues(actx)
			return nil
		}),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := box.Render(ctx, w); err != nil {
			return err
		}
		return run.Render(templ.WithChildren(ctx, templ.Raw("go")), w)
	})
}

// badSignals declares a name Datastar would quietly reinterpret.
type badSignals struct{ counter }

func (badSignals) Signals() map[string]any {
	return map[string]any{"my-signal": 1}
}

func postBody(t *testing.T, srv *httptest.Server, sid, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("action request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SessionHeader, sid)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post action: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestPathsRenderTwoWays pins the id and signal schemes. Both are derived
// from a component's position rather than a counter, which is what makes a
// fragment re-render produce the ids the page already has.
func TestPathsRenderTwoWays(t *testing.T) {
	for _, tc := range []struct {
		p       path
		id, sig string
	}{
		{nil, "shuttle-c", "c"},
		{path{2}, "shuttle-c-2", "c.2"},
		{path{1, 3}, "shuttle-c-1-3", "c.1.3"},
	} {
		if got := tc.p.elementID(); got != tc.id {
			t.Errorf("%v.elementID() = %q, want %q", tc.p, got, tc.id)
		}
		if got := tc.p.namespace(); got != tc.sig {
			t.Errorf("%v.namespace() = %q, want %q", tc.p, got, tc.sig)
		}
	}
}

// TestChildPathDoesNotAliasItsParent: paths are slices, and a parent's path
// outlives the call that derives a child from it.
func TestChildPathDoesNotAliasItsParent(t *testing.T) {
	parent := path{1}
	a, b := parent.child(2), parent.child(3)

	if a.elementID() != "shuttle-c-1-2" || b.elementID() != "shuttle-c-1-3" {
		t.Errorf("child paths alias each other: %q and %q", a.elementID(), b.elementID())
	}
	if parent.elementID() != "shuttle-c-1" {
		t.Errorf("parent path was mutated: %q", parent.elementID())
	}
}

// TestSignalsAreNamespacedToTheInstance covers the sleeper requirement.
// Datastar has one global signal store with no scoping and no collision
// warning, so two components both declaring "query" would silently share
// one value.
func TestSignalsAreNamespacedToTheInstance(t *testing.T) {
	sess := newSession("test", &search{})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// __ifmissing, because a morph re-applies a data attribute whose value
	// changed - without it a re-render would reset what the user just typed.
	for _, want := range []string{
		`data-signals:c.open__ifmissing="false"`,
		`data-signals:c.query__ifmissing="&#34;&#34;"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("missing %q in %q", want, markup)
		}
	}

	// Sorted, so a re-render is byte-identical.
	if strings.Index(markup, "c.open") > strings.Index(markup, "c.query") {
		t.Error("signals are not emitted in sorted order")
	}
}

// TestActionsUploadOnlyTheirOwnSignals: every Datastar action sends the
// whole global store by default, so an unscoped click on a large app
// uploads all of its client state.
func TestActionsUploadOnlyTheirOwnSignals(t *testing.T) {
	sess := newSession("test", &search{})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The exclude half keeps a parent from also uploading its children:
	// filterSignals matches whole dot-paths, so /^c\./ catches c.1.query as
	// readily as c.query.
	if !strings.Contains(markup, `filterSignals: {include: /^c\./, exclude: /^c\.[0-9]+\./}`) {
		t.Errorf("action is not scoped to the component's namespace: %q", markup)
	}
}

// TestFilterExcludesChildrenNotObjectSignals: the exclude pattern has to be
// precise enough to drop a child namespace while leaving a component's own
// object-valued signal alone. Numeric path segments are what make that
// possible, and are why children are numbered rather than named after their
// keys.
func TestFilterExcludesChildrenNotObjectSignals(t *testing.T) {
	filter := filterFor("c")
	exclude := regexp.MustCompile(`exclude: /([^/]+)/`).FindStringSubmatch(filter)
	if exclude == nil {
		t.Fatalf("no exclude pattern in %q", filter)
	}
	re := regexp.MustCompile(exclude[1])

	for path, want := range map[string]bool{
		"c.1.query":       true,  // a child's signal
		"c.12.deep.thing": true,  // a child's nested signal
		"c.query":         false, // the component's own
		"c.filters.name":  false, // its own, object-valued
	} {
		if got := re.MatchString(path); got != want {
			t.Errorf("exclude %q matched %q = %v, want %v", exclude[1], path, got, want)
		}
	}
}

// TestSignalHelpersOnlyWorkInARender.
func TestSignalHelpersOnlyWorkInARender(t *testing.T) {
	sess := newSession("test", &probe{
		check: func(t *testing.T, ctx context.Context) {
			if got := Signal(ctx, "query"); got != "c.query" {
				t.Errorf("Signal = %q, want c.query", got)
			}
			if got := Ref(ctx, "query"); got != "$c.query" {
				t.Errorf("Ref = %q, want $c.query", got)
			}
			if got := ElementID(ctx, "email"); got != "shuttle-c-email" {
				t.Errorf("ElementID = %q, want shuttle-c-email", got)
			}
		},
		t: t,
	})
	if _, err := sess.Render(context.Background()); err != nil {
		t.Fatalf("render: %v", err)
	}

	bare := context.Background()
	if Signal(bare, "query") != "" || Ref(bare, "query") != "" || ElementID(bare, "e") != "" {
		t.Error("helpers returned a value outside a render pass")
	}
}

// probe runs an assertion inside a real render pass.
type probe struct {
	Base
	t     *testing.T
	check func(*testing.T, context.Context)
}

func (p *probe) Render(ctx context.Context) templ.Component {
	p.check(p.t, ctx)
	return templ.Raw("")
}

// TestBadSignalNameFailsTheRender: a name with a hyphen is camel-cased by
// Datastar and a name with a dot becomes a path, so either would end up
// referring to a signal the component cannot address. Better to fail loudly
// than to namespace it wrongly.
func TestBadSignalNameFailsTheRender(t *testing.T) {
	sess := newSession("test", &badSignals{})
	if _, err := sess.Render(context.Background()); err == nil {
		t.Fatal("render accepted a signal name Datastar would rewrite")
	}
}

// TestIDIsScopedAndStable: an explicit id is what a morph reconciles on, so
// it has to survive a re-render unchanged.
func TestIDIsScopedAndStable(t *testing.T) {
	sess := newSession("test", &search{})
	ctx := context.Background()

	first, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}

	if !strings.Contains(first, `id="shuttle-c-query"`) {
		t.Errorf("explicit id missing: %q", first)
	}
	if normalise(first) != normalise(second) {
		t.Errorf("render is not stable:\nfirst  %s\nsecond %s", first, second)
	}
}

// TestNoDuplicateIDsInARender: a duplicate id is excluded from Datastar's
// persistent-id set, which silently drops that subtree to soft matching.
func TestNoDuplicateIDsInARender(t *testing.T) {
	for name, cmp := range map[string]Component{
		"search":  &search{},
		"form":    &form{Err: "required"},
		"counter": &counter{},
	} {
		sess := newSession("test", cmp)
		markup, err := sess.Render(context.Background())
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		if dupes := DuplicateIDs(markup); dupes != nil {
			t.Errorf("%s: duplicate ids %v in %q", name, dupes, markup)
		}
	}
}

// TestDuplicateIDsReportsThem, so the guard above can be trusted.
func TestDuplicateIDsReportsThem(t *testing.T) {
	markup := `<div id="a"><p id="b"></p><p id="a"></p><i id="b"></i><i id="c"></i></div>`
	got := DuplicateIDs(markup)
	if fmt.Sprint(got) != "[a b]" {
		t.Errorf("DuplicateIDs = %v, want [a b]", got)
	}
	if got := DuplicateIDs(`<div id="a"><p id="b"></p></div>`); got != nil {
		t.Errorf("DuplicateIDs on clean markup = %v, want none", got)
	}
}

// TestSignalValuesReachTheAction closes the loop: a client value, sent under
// the component's namespace, arrives at the closure with the namespace
// stripped.
func TestSignalValuesReachTheAction(t *testing.T) {
	c := &search{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	url := clickURLs(t, fragment(t, page))[0]

	if code := postBody(t, srv, sid, url, `{"c":{"query":"hello","open":true}}`); code != http.StatusNoContent {
		t.Fatalf("action: status %d, want 204", code)
	}
	if c.Ran != 1 {
		t.Fatalf("action ran %d times, want 1", c.Ran)
	}
	if got := c.Seen["query"]; got != "hello" {
		t.Errorf("query = %v, want hello", got)
	}
	if got := c.Seen["open"]; got != true {
		t.Errorf("open = %v, want true", got)
	}
}

// TestActionSeesOnlyItsOwnNamespace: filterSignals is a client-side courtesy,
// so the server strips the namespace itself rather than trusting the payload
// to contain only what it asked for.
func TestActionSeesOnlyItsOwnNamespace(t *testing.T) {
	c := &search{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	url := clickURLs(t, fragment(t, page))[0]

	body := `{"c":{"query":"mine"},"other":{"query":"theirs"},"loose":1}`
	if code := postBody(t, srv, sid, url, body); code != http.StatusNoContent {
		t.Fatalf("action: status %d, want 204", code)
	}
	if got := c.Seen["query"]; got != "mine" {
		t.Errorf("query = %v, want mine", got)
	}
	if _, ok := c.Seen["other"]; ok {
		t.Errorf("action saw another namespace: %v", c.Seen)
	}
	if _, ok := c.Seen["loose"]; ok {
		t.Errorf("action saw an unnamespaced signal: %v", c.Seen)
	}
}

// host renders one component as a child, so the child's signals sit a level
// down in the payload rather than at the root of it.
type host struct {
	Base
	child *search
}

func (h *host) Render(ctx context.Context) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return Child(ctx, "one", func() Component { return h.child }).Render(ctx, w)
	})
}

// TestChildActionReadsItsOwnNamespace. A child's signals arrive nested under
// its path - c.1.query, not c.query - so the server has to descend to it. A
// component that worked as a root and silently saw nothing as somebody's
// child is the failure this covers.
func TestChildActionReadsItsOwnNamespace(t *testing.T) {
	c := &search{}
	h := New(func() Component { return &host{child: c} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	// The parent binds nothing, so the only click on the page is the child's.
	url := clickURLs(t, fragment(t, page))[0]

	body := `{"c":{"query":"the parent's","1":{"query":"mine"}}}`
	if code := postBody(t, srv, sid, url, body); code != http.StatusNoContent {
		t.Fatalf("action: status %d, want 204", code)
	}
	if got := c.Seen["query"]; got != "mine" {
		t.Errorf("query = %v, want mine", got)
	}
	if len(c.Seen) != 1 {
		t.Errorf("the child saw more than its own namespace: %v", c.Seen)
	}
}

// TestPayloadsThatDoNotReachTheComponent. Signals are client-controlled, so
// every way a payload can fail to arrive is a request the server still has
// to answer: the action runs, and it simply sees nothing.
func TestPayloadsThatDoNotReachTheComponent(t *testing.T) {
	for name, body := range map[string]string{
		"another root":       `{"other":{"query":"x"}}`,
		"another child":      `{"c":{"2":{"query":"x"}}}`,
		"root is not nested": `{"c":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := &search{}
			h := New(func() Component { return &host{child: c} })
			h.Logger = quietLogger()
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			page, sid := getPage(t, srv)
			url := clickURLs(t, fragment(t, page))[0]

			if code := postBody(t, srv, sid, url, body); code != http.StatusNoContent {
				t.Fatalf("action: status %d, want 204", code)
			}
			if c.Ran != 1 {
				t.Errorf("action ran %d times, want 1", c.Ran)
			}
			if len(c.Seen) != 0 {
				t.Errorf("action saw signals that were not its own: %v", c.Seen)
			}
		})
	}
}

// TestActionWithoutSignalsIsFine: a component declaring none posts no body,
// which is not an error.
func TestActionWithoutSignalsIsFine(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	if code := post(t, srv, sid, clickURLs(t, fragment(t, page))[0]); code != http.StatusNoContent {
		t.Errorf("action with no signals: status %d, want 204", code)
	}
}

// TestStrayGETDoesNotStartASession. A crawler asking for /favicon.ico or
// /robots.txt should not cost a component tree apiece.
func TestStrayGETDoesNotStartASession(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for _, p := range []string{"/favicon.ico", "/robots.txt", "/anything/else"} {
		resp, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}
	if n := h.sessions.len(); n != 0 {
		t.Errorf("%d sessions created by stray GETs", n)
	}
}

// dupes renders the same explicit id twice, which is the failure Debug
// exists to surface.
type dupes struct{ Base }

func (d *dupes) Render(ctx context.Context) templ.Component {
	a := input.New(ID(ctx, input.ID, "same"))
	b := input.New(ID(ctx, input.ID, "same"))
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := a.Render(ctx, w); err != nil {
			return err
		}
		return b.Render(ctx, w)
	})
}

// TestDebugWarnsAboutDuplicateIDs. Datastar reports nothing at all for this,
// so the framework has to.
func TestDebugWarnsAboutDuplicateIDs(t *testing.T) {
	var log strings.Builder
	h := New(func() Component { return &dupes{} })
	h.Logger = slog.New(slog.NewTextHandler(&log, nil))
	h.Debug = true
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	getPage(t, srv)

	if !strings.Contains(log.String(), "duplicate element ids") {
		t.Errorf("no warning about duplicate ids, log was:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "shuttle-c-same") {
		t.Errorf("warning does not name the duplicate id:\n%s", log.String())
	}
}

// TestBadSignalPayloadIsARequestError: signals are client-controlled input.
func TestBadSignalPayloadIsARequestError(t *testing.T) {
	h := New(func() Component { return &search{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	url := clickURLs(t, fragment(t, page))[0]

	if code := postBody(t, srv, sid, url, `{not json`); code != http.StatusBadRequest {
		t.Errorf("undecodable signals: status %d, want 400", code)
	}
}

// indicating is a component with a pending marker on its action, and a
// child of the same kind - two instances, so the test can prove the
// indicators do not share a signal.
type indicating struct {
	Base
	Child bool
}

func (i *indicating) Render(ctx context.Context) templ.Component {
	save := button.New(
		button.Attr("data-attr:disabled", IndicatorRef(ctx, "saving")),
		OnClick(ctx, button.Attr, func(context.Context) error { return nil },
			Indicator("saving")),
	)

	var child templ.Component
	if !i.Child {
		child = Child(ctx, "row", func() Component { return &indicating{Child: true} })
	}

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := save.Render(templ.WithChildren(ctx, templ.Raw("save")), w); err != nil {
			return err
		}
		if child == nil {
			return nil
		}
		return child.Render(ctx, w)
	})
}

// TestIndicatorIsNamespacedPerInstance. Datastar has one global signal
// store, so two instances of the same component sharing an indicator name
// would light each other's spinners.
func TestIndicatorIsNamespacedPerInstance(t *testing.T) {
	sess := newSession("test", &indicating{})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		`data-indicator="ind.c.saving"`,
		`data-indicator="ind.c.1.saving"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("missing %q in %q", want, markup)
		}
	}
}

// TestIndicatorRefNamesTheInstanceSignal. Writing "$ind.c.saving" by hand
// works for a root component and silently watches the wrong signal the
// moment the same component is mounted as a child - which is why reading
// one is a call and not a string.
func TestIndicatorRefNamesTheInstanceSignal(t *testing.T) {
	sess := newSession("test", &indicating{})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		`data-attr:disabled="$ind.c.saving"`,
		`data-attr:disabled="$ind.c.1.saving"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("missing %q in %q", want, markup)
		}
	}

	// Outside a render there is no instance to name.
	if got := IndicatorRef(context.Background(), "saving"); got != "" {
		t.Errorf("IndicatorRef outside a render = %q, want empty", got)
	}
}

// TestIndicatorNeverTravelsToTheServer: the signal exists so the client can
// answer "is this busy" without asking. Rooting it outside c. is what keeps
// it out of every action payload - and out of DecodeSignals, where a
// component field tagged the same would otherwise decode it.
func TestIndicatorNeverTravelsToTheServer(t *testing.T) {
	include := regexp.MustCompile(`include: /([^/]+)/`).
		FindStringSubmatch(filterFor("c"))
	if include == nil {
		t.Fatalf("no include pattern in %q", filterFor("c"))
	}
	re := regexp.MustCompile(include[1])

	sc := &scope{node: &node{}}
	if path := sc.indicatorFor("saving"); re.MatchString(path) {
		t.Errorf("filterSignals uploads the indicator: %q matches %q", path, include[1])
	}
}

// TestIndicatorIsDeclaredOnTheRoot. Datastar creates the signal when it
// initialises data-indicator, but attributes are processed in document
// order - so an expression on the same element that reads the signal first
// finds nothing, and that error takes the rest of that element's attributes
// with it, the click binding included. Declaring it on the root, which is
// written before any of them, is what makes the order options are passed in
// stop mattering.
func TestIndicatorIsDeclaredOnTheRoot(t *testing.T) {
	sess := newSession("test", &indicating{})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	decl := `data-signals:ind.c.saving__ifmissing="false"`
	if !strings.Contains(markup, decl) {
		t.Fatalf("missing %q in %q", decl, markup)
	}

	// Before the element that uses it, or it would not have helped.
	if strings.Index(markup, decl) > strings.Index(markup, `data-indicator=`) {
		t.Error("the declaration comes after the element that reads it")
	}

	// The child's is its own, and declared on the child's root.
	if want := `data-signals:ind.c.1.saving__ifmissing="false"`; !strings.Contains(markup, want) {
		t.Errorf("missing %q", want)
	}
}
