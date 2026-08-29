package shuttle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/input"
)

// recorder stands in for *testing.T so the kit's own failures can be
// asserted on rather than failing this test run.
type recorder struct {
	errs     []string
	fatals   []string
	cleanups []func()
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// Fatalf records rather than stopping the goroutine, so a single test can
// provoke several failures. The kit returns after calling it, so nothing
// downstream runs on a broken state.
func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recorder) Cleanup(fn func()) { r.cleanups = append(r.cleanups, fn) }

func (r *recorder) done() {
	for _, v := range slices.Backward(r.cleanups) {
		v()
	}
}

func (r *recorder) failures() string {
	return strings.Join(append(append([]string{}, r.errs...), r.fatals...), "\n")
}

// todo is a component with the shape a real one has: some state, an action,
// a client signal, and a list.
type todo struct {
	Base
	Items []string
	Done  int
}

func (t *todo) Signals() map[string]any { return map[string]any{"draft": ""} }

func (t *todo) Render(ctx context.Context) templ.Component {
	add := button.New(
		button.Primary,
		ID(ctx, button.ID, "add"),
		OnClick(ctx, button.Attr, func(actx context.Context) error {
			var f struct {
				Draft string `json:"draft"`
			}
			if err := DecodeSignals(actx, &f); err != nil {
				return err
			}
			if f.Draft != "" {
				t.Items = append(t.Items, f.Draft)
			}
			return nil
		}),
	)
	clear := button.New(
		ID(ctx, button.ID, "clear"),
		OnClick(ctx, button.Attr, func(context.Context) error {
			t.Items, t.Done = nil, t.Done+1
			return nil
		}),
	)
	box := input.New(ID(ctx, input.ID, "draft"), Bind(ctx, input.Attr, "draft"))

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := box.Render(ctx, w); err != nil {
			return err
		}
		if err := add.Render(templ.WithChildren(ctx, templ.Raw("add")), w); err != nil {
			return err
		}
		if err := clear.Render(templ.WithChildren(ctx, templ.Raw("clear")), w); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, `<p id="done">%d</p><ul>`, t.Done); err != nil {
			return err
		}
		for _, item := range t.Items {
			if _, err := fmt.Fprintf(w, `<li class="item">%s</li>`, templ.EscapeString(item)); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "</ul>")
		return err
	})
}

// TestKitDrivesAComponent is the kit doing the job it exists for, and reads
// the way a user's own test would.
func TestKitDrivesAComponent(t *testing.T) {
	live := Test(t, &todo{})

	live.Assert().
		Count("li.item", 0).
		Text("#done", "0").
		NoDuplicateIDs()

	live.Signal("draft", "buy milk").Click("#shuttle-c-add")
	live.Assert().
		Count("li.item", 1).
		Text("li.item", "buy milk")

	live.Signal("draft", "write tests").Click("#shuttle-c-add")
	live.Assert().Count("li.item", 2)

	live.Click("#shuttle-c-clear")
	live.Assert().
		Count("li.item", 0).
		Text("#done", "1")

	// The component itself is right there to assert on, which is often
	// clearer than going through the markup.
	if got := live.Component().(*todo).Done; got != 1 {
		t.Errorf("Done = %d, want 1", got)
	}
}

// TestKitAssertsOnWhatTheClientWouldReceive: the markup comes from the
// patch the session pushed, not from a render done for the test. A
// component whose action pushes nothing has to fail here too.
func TestKitAssertsOnWhatTheClientWouldReceive(t *testing.T) {
	live := Test(t, &todo{})
	before := live.HTML()

	live.Signal("draft", "something").Click("#shuttle-c-add")

	if live.HTML() == before {
		t.Error("the kit did not pick up the patch the action caused")
	}
	if !strings.Contains(live.HTML(), "something") {
		t.Errorf("markup is not the post-action render: %q", live.HTML())
	}
}

// TestKitSignalsReachTheAction.
func TestKitSignalsReachTheAction(t *testing.T) {
	live := Test(t, &todo{})

	// No signal set: the action sees an empty draft and adds nothing.
	live.Click("#shuttle-c-add")
	live.Assert().Count("li.item", 0)

	live.Signal("draft", "now there is one").Click("#shuttle-c-add")
	live.Assert().Count("li.item", 1)
}

// TestKitRunsTheLifecycle: Mount and HandleParams, as a page load would.
func TestKitRunsTheLifecycle(t *testing.T) {
	live := Test(t, &lifecycle{})
	c := live.Component().(*lifecycle)

	if c.Mounted != 1 || c.Params != 1 {
		t.Errorf("Mount ran %d times, HandleParams %d, want 1 each", c.Mounted, c.Params)
	}

	live.Params("filter=open")
	if c.Params != 2 {
		t.Errorf("HandleParams ran %d times after a param change, want 2", c.Params)
	}
	if c.Filter != "open" {
		t.Errorf("Filter = %q, want open", c.Filter)
	}
	live.Assert().Text("#filter", "open")
}

type lifecycle struct {
	Base
	Mounted, Params int
	Filter          string
}

func (l *lifecycle) Mount(context.Context, Params) error { l.Mounted++; return nil }

func (l *lifecycle) HandleParams(_ context.Context, p Params) error {
	l.Params++
	l.Filter = p.Get("filter")
	return nil
}

func (l *lifecycle) Render(context.Context) templ.Component {
	return templ.Raw(fmt.Sprintf(`<p id="filter">%s</p>`, templ.EscapeString(l.Filter)))
}

// TestKitDeliversPublishedMessages, so HandleInfo can be tested without a
// second page.
func TestKitDeliversPublishedMessages(t *testing.T) {
	live := Test(t, &listener{})
	live.Publish("lobby", "hello")

	live.Assert().Text("#heard", "hello")
}

type listener struct {
	Base
	Heard string
}

func (l *listener) Mount(context.Context, Params) error { return l.Subscribe("lobby") }

func (l *listener) HandleInfo(_ context.Context, msg any) error {
	l.Heard = fmt.Sprint(msg)
	return nil
}

func (l *listener) Render(context.Context) templ.Component {
	return templ.Raw(fmt.Sprintf(`<p id="heard">%s</p>`, templ.EscapeString(l.Heard)))
}

// TestKitReportsAMissingElement rather than panicking on a nil node, since
// a selector typo is the most common thing to get wrong.
func TestKitReportsAMissingElement(t *testing.T) {
	r := &recorder{}
	live := Test(r, &todo{})
	defer r.done()

	live.Click("#nothing-here")

	if len(r.fatals) == 0 {
		t.Fatal("clicking a missing element did not fail the test")
	}
	if !strings.Contains(r.fatals[0], "#nothing-here") {
		t.Errorf("failure does not name the selector: %q", r.fatals[0])
	}
}

// TestKitReportsAnElementWithNoBinding, which is the other easy mistake:
// the selector matched, but not the thing that handles clicks.
func TestKitReportsAnElementWithNoBinding(t *testing.T) {
	r := &recorder{}
	live := Test(r, &todo{})
	defer r.done()

	live.Click("#done")

	if len(r.fatals) == 0 {
		t.Fatal("clicking an unbound element did not fail the test")
	}
	if !strings.Contains(r.fatals[0], "no click binding") {
		t.Errorf("failure is not about the missing binding: %q", r.fatals[0])
	}
}

// TestAssertionsFailWhenTheyShould. An assertion that cannot fail is worse
// than none.
func TestAssertionsFailWhenTheyShould(t *testing.T) {
	for name, check := range map[string]func(a *Assert){
		"Text":         func(a *Assert) { a.Text("#done", "99") },
		"TextContains": func(a *Assert) { a.TextContains("#done", "99") },
		"Attr":         func(a *Assert) { a.Attr("#done", "id", "wrong") },
		"Count":        func(a *Assert) { a.Count("li.item", 7) },
		"Exists":       func(a *Assert) { a.Exists("#absent") },
		"Missing":      func(a *Assert) { a.Missing("#done") },
		"Contains":     func(a *Assert) { a.Contains("not in there") },
		"URL":          func(a *Assert) { a.URL("/elsewhere") },
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			live := Test(r, &todo{})
			defer r.done()

			check(live.Assert())

			if len(r.errs) == 0 {
				t.Errorf("%s passed when it should have failed", name)
			}
		})
	}
}

// TestAssertionsPassWhenTheyShould, the other half.
func TestAssertionsPassWhenTheyShould(t *testing.T) {
	r := &recorder{}
	live := Test(r, &todo{})
	defer r.done()

	live.Assert().
		Text("#done", "0").
		TextContains("#done", "0").
		Attr("#shuttle-c-add", "type", "button").
		Count("li.item", 0).
		Exists("#shuttle-c-draft").
		Missing("#absent").
		Contains(`data-signals:c.draft__ifmissing`).
		NoDuplicateIDs().
		URL("/")

	if f := r.failures(); f != "" {
		t.Errorf("assertions failed on a healthy component:\n%s", f)
	}
}

// TestKitFiresASubmit. The change/submit split is a form's whole shape, so
// a kit that could only click would leave the committing half untestable.
func TestKitFiresASubmit(t *testing.T) {
	live := Test(t, &subscribe{})
	live.Assert().Text("#sent", "0")

	live.Submit("#shuttle-c-join")
	live.Assert().Text("#sent", "1")
}

// subscribe renders its own <form>, which is what OnSubmit is for - Loom
// has no form component, and the binding is generic over any package's
// option type rather than only Loom's.
type subscribe struct {
	Base
	Sent int
}

// formOpts is the option type of a package that is not Loom's, and
// formAttr its Attrs: the pair the generic bindings infer from.
type formOpts struct{ attrs string }

func formAttr(key string, val ...string) func(*formOpts) {
	return func(o *formOpts) {
		if len(val) == 0 {
			o.attrs += " " + key
			return
		}
		o.attrs += fmt.Sprintf(" %s=%q", key, val[0])
	}
}

func (s *subscribe) Render(ctx context.Context) templ.Component {
	var o formOpts
	OnSubmit(ctx, formAttr, func(context.Context) error {
		s.Sent++
		return nil
	})(&o)
	id := ElementID(ctx, "join")

	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w,
			`<form id=%q%s><button>join</button></form><p id="sent">%d</p>`,
			id, o.attrs, s.Sent)
		return err
	})
}

// TestKitPatchesCarryWhatIsNotInTheMarkup: a stream patches its container
// rather than re-rendering the component, so a test asserting on HTML alone
// would never see a streamed item. Patches is how those are asserted on.
func TestKitPatchesCarryWhatIsNotInTheMarkup(t *testing.T) {
	live := Test(t, &feed{})
	live.Click("button")

	var streamed string
	for _, p := range live.Patches() {
		if strings.Contains(p, `id="shuttle-c-log-1"`) {
			streamed = p
		}
	}
	if streamed == "" {
		t.Fatalf("no streamed item in the patches: %q", live.HTML())
	}
	if !strings.Contains(streamed, "posted") {
		t.Errorf("streamed item = %q, want it to carry the text", streamed)
	}

	// Patches drains, so a second action's patches are its own rather than
	// everything the session has ever pushed.
	if got := live.Patches(); len(got) != 0 {
		t.Errorf("Patches returned %d patches again, want it to have drained", len(got))
	}
	live.Click("button")
	if got := live.Patches(); len(got) == 0 {
		t.Error("the second action's patches were lost")
	}
}

// TestKitReportsAFailingAction. The action runs on the session's goroutine,
// so an error it returns has nowhere to surface on its own - without this
// the test would carry on and fail later on an assertion, naming the wrong
// thing.
func TestKitReportsAFailingAction(t *testing.T) {
	r := &recorder{}
	live := Test(r, &brittle{})
	defer r.done()

	live.Click("button")

	if len(r.fatals) == 0 {
		t.Fatal("an action that returned an error did not fail the test")
	}
	if !strings.Contains(r.fatals[0], "acting") || !strings.Contains(r.fatals[0], "no") {
		t.Errorf("failure does not say what went wrong: %q", r.fatals[0])
	}
}

type brittle struct{ Base }

func (b *brittle) Render(ctx context.Context) templ.Component {
	boom := button.New(OnClick(ctx, button.Attr, func(context.Context) error {
		return errors.New("no")
	}))
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return boom.Render(templ.WithChildren(ctx, templ.Raw("boom")), w)
	})
}

// TestKitCatchesDuplicateIDs, the failure Datastar reports nothing about.
func TestKitCatchesDuplicateIDs(t *testing.T) {
	r := &recorder{}
	live := Test(r, &dupes{})
	defer r.done()

	live.Assert().NoDuplicateIDs()

	if len(r.errs) == 0 {
		t.Fatal("duplicate ids passed the assertion")
	}
	if !strings.Contains(r.errs[0], "shuttle-c-same") {
		t.Errorf("failure does not name the duplicate: %q", r.errs[0])
	}
}

// TestSelectors covers the selector engine on its own, since every
// assertion above depends on it.
func TestSelectors(t *testing.T) {
	markup := `<div id="root" data-shuttle="component">
		<ul class="list"><li class="item">one</li><li class="item hot">two</li></ul>
		<button data-ui="button" type="button">go</button>
		<span>loose</span>
	</div>`

	for _, tc := range []struct {
		sel  string
		want int
	}{
		{"li", 2},
		{".item", 2},
		{"li.hot", 1},
		{"#root", 1},
		{"[data-ui=button]", 1},
		{"[data-shuttle]", 1},
		{"button[type=button]", 1},
		{"ul li", 2},
		{"#root ul .hot", 1},
		{"div span", 1},
		{"table", 0},
		{"li.missing", 0},
		{"[data-ui=nope]", 0},
	} {
		got, err := selectAll(markup, tc.sel)
		if err != nil {
			t.Fatalf("%q: %v", tc.sel, err)
		}
		if len(got) != tc.want {
			t.Errorf("%q matched %d elements, want %d", tc.sel, len(got), tc.want)
		}
	}
}

// TestTextIsCollapsed, so assertions can be written the way the markup
// reads rather than the way it is indented.
func TestTextIsCollapsed(t *testing.T) {
	found, err := selectAll("<p id=\"x\">  hello\n\t  world  </p>", "#x")
	if err != nil || len(found) != 1 {
		t.Fatalf("select: %v", err)
	}
	if got := textOf(found[0]); got != "hello world" {
		t.Errorf("textOf = %q, want %q", got, "hello world")
	}
}

// TestTheKitModelsAStreamTheWayTheBrowserDoes covers two things the kit
// used to get wrong, both of which made a streaming component untestable
// without anyone being told.
//
// A stream's append names the container and carries one item, so applying
// every patch as an outer replace swapped the container for the item -
// after which every later assertion was about a document the browser never
// had. And the component's own re-render then overwrote the container, so
// items streamed before it disappeared, which is precisely what
// data-ignore-morph exists to prevent.
func TestTheKitModelsAStreamTheWayTheBrowserDoes(t *testing.T) {
	live := Test(t, &feed{})

	live.Click("button")
	live.Assert().
		Text("#sent", "1").
		Exists("ul#shuttle-c-log").
		Count("ul li", 1)

	// The second click re-renders the component as well as appending, and
	// the first item has to survive that.
	live.Click("button")
	live.Assert().
		Text("#sent", "2").
		Count("ul li", 2)
}

// TestSelectorFindsNestedMatches. The old walk stopped searching inside
// anything that matched, so a .item nested in a .item was invisible -
// Count undercounted and Missing passed over elements that were there.
func TestSelectorFindsNestedMatches(t *testing.T) {
	markup := `<div class="item"><div class="item"><span class="item">x</span></div></div>` +
		`<ul id="list"><li><ul><li>deep</li></ul></li></ul>`

	for sel, want := range map[string]int{
		".item":       3,
		"li":          2,
		"#list li":    2,
		".item .item": 2,
	} {
		got, err := selectAll(markup, sel)
		if err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		if len(got) != want {
			t.Errorf("%s matched %d elements, want %d", sel, len(got), want)
		}
	}
}

// TestSelectorRefusesWhatItCannotParse. Unsupported syntax used to parse
// into something that matched nothing, which turns Missing() and
// Count(x, 0) into tests of the engine's limits rather than of the page.
func TestSelectorRefusesWhatItCannotParse(t *testing.T) {
	for _, sel := range []string{
		"",
		"ul > li",
		"a, b",
		"li:first-child",
		"[href^=http]",
		"[unterminated",
		"#",
		".",
	} {
		if _, err := selectAll("<p>x</p>", sel); err == nil {
			t.Errorf("selector %q was accepted; it should be refused loudly", sel)
		}
	}
}

// TestSelectorCompoundsClassesAndQuotedValues.
func TestSelectorCompoundsClassesAndQuotedValues(t *testing.T) {
	markup := `<li class="item hot">a</li><li class="hot">b</li>` +
		`<p title="hello world">c</p>`

	got, err := selectAll(markup, "li.item.hot")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("li.item.hot matched %d, want 1 - the compound must not collapse to its last class", len(got))
	}

	got, err = selectAll(markup, `[title="hello world"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf(`[title="hello world"] matched %d, want 1 - the space must not split the step`, len(got))
	}
}

// TestTheKitRunsUnderSynctest: a component driven by Every was untestable
// without wall-clock sleeps, because nothing could advance time. Wrapping
// a kit test in synctest.Test fixes that - the session's goroutine, its
// timers and the kit all live in the bubble, so time.Sleep advances the
// fake clock and the ticks are exact rather than raced.
func TestTheKitRunsUnderSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &clock{}
		live := Test(t, c)

		// Ten intervals pass instantly and deterministically.
		time.Sleep(52 * time.Millisecond)
		synctest.Wait()

		if got := c.count(); got != 10 {
			t.Errorf("ticks = %d, want exactly 10 - fake time does not race", got)
		}
		live.Assert().Exists("#ticks")
	})
}
