package shuttle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
)

// row is a child with state of its own.
type row struct {
	Base
	Label   string
	Ticks   int
	dropped bool
}

func (r *row) Signals() map[string]any { return map[string]any{"draft": ""} }

func (r *row) Unmount(context.Context) { r.dropped = true }

func (r *row) Render(ctx context.Context) templ.Component {
	tick := button.New(
		OnClick(ctx, button.Attr, func(context.Context) error {
			r.Ticks++
			return nil
		}),
	)
	tell := button.New(
		OnClick(ctx, button.Attr, func(actx context.Context) error {
			return r.Emit(actx, "picked", r.Label)
		}),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := fmt.Fprintf(w, `<span class="row">%s:%d</span>`, r.Label, r.Ticks); err != nil {
			return err
		}
		if err := tick.Render(templ.WithChildren(ctx, templ.Raw("tick")), w); err != nil {
			return err
		}
		return tell.Render(templ.WithChildren(ctx, templ.Raw("tell")), w)
	})
}

// list renders a keyed child per label and listens for what they emit.
type list struct {
	Base
	Labels []string
	Heard  []string
	Bumps  int

	// built records the labels the factory actually constructed, which is
	// how the tests tell a re-used instance from a remounted one.
	built []string
}

func (l *list) HandleEvent(_ context.Context, name string, payload any) error {
	l.Heard = append(l.Heard, fmt.Sprintf("%s:%v", name, payload))
	return nil
}

func (l *list) Render(ctx context.Context) templ.Component {
	bump := button.New(
		OnClick(ctx, button.Attr, func(context.Context) error {
			l.Bumps++
			return nil
		}),
	)
	labels := append([]string(nil), l.Labels...)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := fmt.Fprintf(w, `<p id="bumps">%d</p>`, l.Bumps); err != nil {
			return err
		}
		if err := bump.Render(templ.WithChildren(ctx, templ.Raw("bump")), w); err != nil {
			return err
		}
		for _, label := range labels {
			child := Child(ctx, label, func() Component {
				l.built = append(l.built, label)
				return &row{Label: label}
			})
			if err := child.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

// rowNode digs a mounted child out of the tree.
func rowNode(t *testing.T, sess *Session, nodeID string) *row {
	t.Helper()
	n, ok := sess.node(nodeID)
	if !ok {
		t.Fatalf("no component mounted at %q", nodeID)
	}
	r, ok := n.cmp.(*row)
	if !ok {
		t.Fatalf("component at %q is %T, want *row", nodeID, n.cmp)
	}
	return r
}

// mountedNodes lists every mounted component id.
func mountedNodes(sess *Session) []string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ids := make([]string, 0, len(sess.nodes))
	for id := range sess.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestChildrenGetTheirOwnIdentity: own morph target, own signal namespace,
// own action table. Without all three a child cannot re-render alone.
func TestChildrenGetTheirOwnIdentity(t *testing.T) {
	sess := newSession("test", &list{Labels: []string{"a", "b"}})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		`<div id="shuttle-c" data-shuttle="component"`,
		`<div id="shuttle-c-1" data-shuttle="component"`,
		`<div id="shuttle-c-2" data-shuttle="component"`,
		// Each child's signals sit in its own namespace, so two children
		// declaring "draft" do not silently share one value.
		`data-signals:c.1.draft__ifmissing=`,
		`data-signals:c.2.draft__ifmissing=`,
		// Actions name the component that owns them.
		`/act/c-1/1-a1`,
		`/act/c-2/1-a1`,
		`/act/c/1-a1`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("missing %q in %q", want, markup)
		}
	}

	if dupes := DuplicateIDs(markup); dupes != nil {
		t.Errorf("nesting produced duplicate ids: %v", dupes)
	}
}

// TestKeyIsTheIdentityRule: the same key keeps the same instance and its
// state across a parent's re-renders.
func TestKeyIsTheIdentityRule(t *testing.T) {
	l := &list{Labels: []string{"a", "b"}}
	sess := newSession("test", l)
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	rowNode(t, sess, "c-1").Ticks = 7

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if got := rowNode(t, sess, "c-1").Ticks; got != 7 {
		t.Errorf("child state lost across a parent re-render: Ticks = %d, want 7", got)
	}
	if got := fmt.Sprint(l.built); got != "[a b]" {
		t.Errorf("factory ran again for an existing key: built = %v", got)
	}
}

// TestNewKeyMountsAndDroppedKeyUnmounts. A child the parent stopped
// rendering has to be unmounted, or its state and action table stay alive
// for the life of the session.
func TestNewKeyMountsAndDroppedKeyUnmounts(t *testing.T) {
	l := &list{Labels: []string{"a", "b"}}
	sess := newSession("test", l)
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	dropped := rowNode(t, sess, "c-1")

	l.Labels = []string{"b", "c"}
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}

	if !dropped.dropped {
		t.Error("a child the parent stopped rendering was not unmounted")
	}
	if got := mountedNodes(sess); fmt.Sprint(got) != "[shuttle-c shuttle-c-2 shuttle-c-3]" {
		t.Errorf("mounted components = %v", got)
	}
	// "b" kept its slot, so its ids did not move when "a" went away.
	if got := rowNode(t, sess, "c-2").Label; got != "b" {
		t.Errorf("surviving child moved: c-2 is %q, want b", got)
	}
	if got := fmt.Sprint(l.built); got != "[a b c]" {
		t.Errorf("built = %v, want [a b c]", got)
	}
}

// TestChildActionRerendersOnlyTheChild is the scoped-render requirement: an
// event on a child must not redraw its parent, or nesting buys nothing.
func TestChildActionRerendersOnlyTheChild(t *testing.T) {
	l := &list{Labels: []string{"a", "b"}}
	h := New(func() Component { return l })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	sess, ok := h.sessions.get(sid)
	if !ok {
		t.Fatal("session not registered")
	}

	// The first child's tick button.
	url := childActionURL(t, fragment(t, page), "c-1")
	if code := post(t, srv, sid, url); code != http.StatusNoContent {
		t.Fatalf("child action: status %d", code)
	}

	evt := stream.event(t)
	if !strings.Contains(evt, "data: selector #shuttle-c-1") {
		t.Errorf("patch did not target the child: %q", evt)
	}
	if strings.Contains(evt, `id="bumps"`) {
		t.Errorf("the child's event redrew its parent: %q", evt)
	}
	if got := rowNode(t, sess, "c-1").Ticks; got != 1 {
		t.Errorf("child state = %d, want 1", got)
	}
}

// TestParentActionRedrawsItsChildren, because a parent's markup contains
// them - and they must keep their state while it does.
func TestParentActionRedrawsItsChildren(t *testing.T) {
	l := &list{Labels: []string{"a"}}
	h := New(func() Component { return l })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	sess, _ := h.sessions.get(sid)
	rowNode(t, sess, "c-1").Ticks = 3

	if code := post(t, srv, sid, childActionURL(t, fragment(t, page), "c")); code != http.StatusNoContent {
		t.Fatalf("parent action: status %d", code)
	}

	evt := stream.event(t)
	if !strings.Contains(evt, "data: selector #shuttle-c\n") {
		t.Errorf("patch did not target the parent: %q", evt)
	}
	if !strings.Contains(evt, `<p id="bumps">1</p>`) {
		t.Errorf("parent state not in patch: %q", evt)
	}
	if !strings.Contains(evt, "a:3") {
		t.Errorf("child lost its state when the parent re-rendered: %q", evt)
	}
}

// TestEmitReachesTheNearestReceiver, and re-renders it.
func TestEmitReachesTheNearestReceiver(t *testing.T) {
	l := &list{Labels: []string{"a", "b"}}
	h := New(func() Component { return l })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// The second child's tell button, which emits.
	url := childActionURLs(t, fragment(t, page), "c-2")[1]
	if code := post(t, srv, sid, url); code != http.StatusNoContent {
		t.Fatalf("emit action: status %d", code)
	}

	if got := fmt.Sprint(l.Heard); got != "[picked:b]" {
		t.Errorf("parent heard %v, want [picked:b]", got)
	}

	// Both the emitting child and the parent it notified are patched.
	targets := map[string]bool{}
	for range 2 {
		evt := stream.event(t)
		for _, id := range []string{"#shuttle-c\n", "#shuttle-c-2"} {
			if strings.Contains(evt, "data: selector "+id) {
				targets[id] = true
			}
		}
	}
	if !targets["#shuttle-c\n"] {
		t.Error("the receiving parent was not re-rendered")
	}
}

// TestEmitWithoutAReceiver reports rather than vanishing.
func TestEmitWithoutAReceiver(t *testing.T) {
	sess := newSession("test", &orphan{})
	ctx := context.Background()
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}

	n, _ := sess.node("c-1")
	r := n.cmp.(*row)
	if err := r.Emit(ctx, "picked", "x"); !errors.Is(err, ErrNoReceiver) {
		t.Errorf("Emit = %v, want ErrNoReceiver", err)
	}
}

// TestEmitBeforeMount. A component built but not yet mounted has no node to
// walk up from, so the ancestor search has nothing to search - it reports
// rather than dereferencing a nil.
func TestEmitBeforeMount(t *testing.T) {
	var r row
	if err := r.Emit(context.Background(), "picked", "x"); !errors.Is(err, ErrNotMounted) {
		t.Errorf("Emit = %v, want ErrNotMounted", err)
	}
}

// orphan renders a child but handles nothing it emits.
type orphan struct{ Base }

func (o *orphan) Render(ctx context.Context) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return Child(ctx, "only", func() Component { return &row{Label: "only"} }).Render(ctx, w)
	})
}

// TestSessionCloseUnmountsTheWholeTree.
func TestSessionCloseUnmountsTheWholeTree(t *testing.T) {
	sess := newSession("test", &list{Labels: []string{"a", "b"}})
	ctx := context.Background()
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}

	kids := []*row{rowNode(t, sess, "c-1"), rowNode(t, sess, "c-2")}
	sess.close(ctx)

	for i, k := range kids {
		if !k.dropped {
			t.Errorf("child %d was not unmounted with the session", i)
		}
	}
	if got := mountedNodes(sess); len(got) != 0 {
		t.Errorf("components still mounted after close: %v", got)
	}
}

// TestChildRendersStandaloneAsItDidInThePage: the scoped re-render sends
// only the child's markup, so it has to match what the page already has or
// the morph churns.
func TestChildRendersStandaloneAsItDidInThePage(t *testing.T) {
	sess := newSession("test", &list{Labels: []string{"a", "b"}})
	ctx := context.Background()

	page, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	n, ok := sess.node("c-2")
	if !ok {
		t.Fatal("second child not mounted")
	}
	alone, err := n.render(ctx)
	if err != nil {
		t.Fatalf("child render: %v", err)
	}

	// Find the child's markup inside the page and compare it.
	start := strings.Index(page, `<div id="shuttle-c-2"`)
	if start < 0 {
		t.Fatalf("child not in page: %q", page)
	}
	embedded := page[start : start+len(alone)]

	if normalise(embedded) != normalise(alone) {
		t.Errorf("child renders differently alone:\nin page %s\nalone   %s", embedded, alone)
	}
}

// TestChildrenSeeTheURL. A component whose state comes from its parameters
// - a table reading its filter and page - is usually somebody's child, and
// would otherwise work as a root and render nothing as a child.
func TestChildrenSeeTheURL(t *testing.T) {
	h := New(func() Component { return &urlParent{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/?filter=open")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Present at first render, when the child is mounted mid-render.
	if !strings.Contains(string(body), `<span id="child-filter">open</span>`) {
		t.Errorf("the child never saw the mount-time params: %s", body)
	}

	sid := sessionRE.FindStringSubmatch(string(body))
	sess, _ := h.sessions.get(sid[1])
	stream := openStream(t, srv, sid[1])

	// And again when the URL changes without a remount.
	if code := postBody(t, srv, sid[1], routePrefix+"/nav", `{"url":"/?filter=closed"}`); code != http.StatusNoContent {
		t.Fatalf("nav: status %d", code)
	}
	if evt := stream.event(t); !strings.Contains(evt, `<span id="child-filter">closed</span>`) {
		t.Errorf("the child did not react to the URL change: %q", evt)
	}
	_ = sess
}

// TestChildURLStateReachesTheAddressBar: the component that owns a page's
// filters is usually a child, so its QueryParams has to count.
func TestChildURLStateReachesTheAddressBar(t *testing.T) {
	sess := newSession("test", &urlParent{})
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	sess.url = sess.path(nil)

	child, ok := sess.node("c-1")
	if !ok {
		t.Fatal("child not mounted")
	}
	kid := child.cmp.(*urlChild)

	if err := kid.Do(func(context.Context) error {
		kid.Filter = "mine"
		return nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return sess.currentURL() == "/?filter=mine"
	}, "the child's state to reach the address bar")
}

// urlParent renders a child that reads and writes the URL.
type urlParent struct{ Base }

func (p *urlParent) Render(ctx context.Context) templ.Component {
	return Child(ctx, "kid", func() Component { return &urlChild{} })
}

type urlChild struct {
	Base
	Filter string
}

func (c *urlChild) HandleParams(_ context.Context, p Params) error {
	c.Filter = p.Get("filter")
	return nil
}

func (c *urlChild) QueryParams() url.Values {
	if c.Filter == "" {
		return nil
	}
	return url.Values{"filter": {c.Filter}}
}

func (c *urlChild) Render(context.Context) templ.Component {
	return templ.Raw(`<span id="child-filter">` + templ.EscapeString(c.Filter) + `</span>`)
}

// TestChildOutsideARenderIsInert, rather than panicking on a nil parent.
func TestChildOutsideARenderIsInert(t *testing.T) {
	c := Child(context.Background(), "k", func() Component { return &row{} })
	if got := render(t, c); got != "" {
		t.Errorf("Child rendered %q outside a session, want nothing", got)
	}
}

// childActionURL returns a component's first action endpoint.
func childActionURL(t *testing.T, markup, nodeID string) string {
	t.Helper()
	return childActionURLs(t, markup, nodeID)[0]
}

// childActionURLs returns the action endpoints belonging to one component,
// in document order.
func childActionURLs(t *testing.T, markup, nodeID string) []string {
	t.Helper()
	var out []string
	for _, u := range clickURLs(t, markup) {
		if strings.Contains(u, "/"+nodeID+"/") {
			out = append(out, u)
		}
	}
	if out == nil {
		t.Fatalf("no actions for %q in %q", nodeID, markup)
	}
	return out
}
