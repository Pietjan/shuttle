package live_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

// people is the set a combobox narrows. It stays on the server: the point
// of filter-as-you-type over a stateful layer is that the browser never
// receives the whole list.
var people = []string{"Ada Lovelace", "Alan Turing", "Grace Hopper", "Ada Byron"}

func findPeople(_ context.Context, q string) ([]live.Choice, error) {
	var out []live.Choice
	for _, p := range people {
		if q == "" || strings.Contains(strings.ToLower(p), strings.ToLower(q)) {
			out = append(out, live.Choice{Value: p, Label: p})
		}
	}
	return out, nil
}

// page hosts a combobox as a child, which is how one is used.
type page struct {
	shuttle.Base
	box    *live.Combobox
	Picked string
}

func newPage(box *live.Combobox) *page {
	p := &page{box: box}
	if p.box.OnSelect == nil {
		p.box.OnSelect = func(ctx context.Context, c live.Choice) error {
			p.Picked = c.Value
			// Selecting is the child's action, so it re-renders the child.
			// A parent that wants to show the result has to say so - that is
			// scoped re-render doing its job, not an oversight.
			return p.Push(ctx)
		}
	}
	return p
}

func (p *page) Render(ctx context.Context) templ.Component {
	child := shuttle.Child(ctx, "box", func() shuttle.Component { return p.box })
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := child.Render(ctx, w); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, `<p id="picked">%s</p>`, templ.EscapeString(p.Picked))
		return err
	})
}

// TestComboboxNarrowsOnTheServer is the case Loom names as missing: a list
// filtered as you type, where the filtering happens where the data is.
func TestComboboxNarrowsOnTheServer(t *testing.T) {
	box := &live.Combobox{Search: findPeople, Placeholder: "Find someone…"}
	l := shuttle.Test(t, newPage(box))

	// Nothing has been searched yet, so no rows.
	l.Assert().Count("[data-shuttle-rove-item]", 0)

	l.Signal("query", "ada").Change("[data-shuttle-rove-field]")
	l.Assert().
		Count("[data-shuttle-rove-item]", 2).
		TextContains("[data-shuttle-rove-item]", "Ada")

	l.Signal("query", "grace").Change("[data-shuttle-rove-field]")
	l.Assert().
		Count("[data-shuttle-rove-item]", 1).
		Text("[data-shuttle-rove-item]", "Grace Hopper")

	l.Signal("query", "nobody").Change("[data-shuttle-rove-field]")
	l.Assert().Count("[data-shuttle-rove-item]", 0)
}

// TestComboboxSelectionRunsAClosureOverTheRow. No value is encoded into
// the markup and decoded back out of a request - the row's handler simply
// closes over its own choice.
func TestComboboxSelectionRunsAClosureOverTheRow(t *testing.T) {
	box := &live.Combobox{Search: findPeople}
	p := newPage(box)
	l := shuttle.Test(t, p)

	l.Signal("query", "grace").Change("[data-shuttle-rove-field]")
	l.Click("[data-shuttle-rove-item]")

	if got := box.Selected(); got == nil || got.Value != "Grace Hopper" {
		t.Fatalf("Selected() = %v, want Grace Hopper", got)
	}
	if p.Picked != "Grace Hopper" {
		t.Errorf("OnSelect saw %q", p.Picked)
	}
	l.Assert().Text("#picked", "Grace Hopper")
}

// TestComboboxIsDebouncedAndBound: the query is client-side state, so
// typing costs nothing until the debounce expires.
func TestComboboxIsDebouncedAndBound(t *testing.T) {
	box := &live.Combobox{Search: findPeople, Debounce: 150 * time.Millisecond}
	l := shuttle.Test(t, newPage(box))

	l.Assert().
		Contains("data-on:input__debounce.150ms").
		Contains(`data-bind="c.1.query"`).
		Contains(`data-signals:c.1.query__ifmissing`)
}

// TestComboboxKeyboardWiring: the arrow keys move real focus, which is what
// keeps Enter native. The rows are buttons, not painted-on listbox options.
func TestComboboxKeyboardWiring(t *testing.T) {
	box := &live.Combobox{Search: findPeople}
	l := shuttle.Test(t, newPage(box))
	l.Signal("query", "ada").Change("[data-shuttle-rove-field]")

	l.Assert().
		Exists("[data-shuttle-roving]").
		Exists("[data-shuttle-rove-field]").
		Count("button[data-shuttle-rove-item]", 2)

	if strings.Contains(l.HTML(), `role="option"`) {
		t.Error("rows carry listbox roles they do not honour")
	}
}

// TestComboboxRowsKeepStableIDs. The list is re-rendered on every
// keystroke, so the rows a morph lands on have to be the rows already in
// the page - otherwise focus goes back to nothing mid-typing.
func TestComboboxRowsKeepStableIDs(t *testing.T) {
	box := &live.Combobox{Search: findPeople}
	l := shuttle.Test(t, newPage(box))

	l.Signal("query", "ada").Change("[data-shuttle-rove-field]")
	first := ids(t, l.HTML())

	l.Signal("query", "ada ").Change("[data-shuttle-rove-field]")
	second := ids(t, l.HTML())

	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Errorf("row ids moved between searches:\n%v\n%v", first, second)
	}
	if len(first) == 0 {
		t.Error("rows have no ids at all, so a morph cannot match them")
	}
	l.Assert().NoDuplicateIDs()
}

func ids(t *testing.T, markup string) []string {
	t.Helper()
	var out []string
	for line := range strings.SplitSeq(markup, "id=\"") {
		if i := strings.IndexByte(line, '"'); i > 0 && strings.Contains(line[:i], "-opt-") {
			out = append(out, line[:i])
		}
	}
	return out
}

// TestComboboxSurvivesAFailingSearch: a search that errors shows a message
// rather than taking the page down.
func TestComboboxSurvivesAFailingSearch(t *testing.T) {
	boom := errors.New("index unavailable")
	box := &live.Combobox{
		Search: func(context.Context, string) ([]live.Choice, error) { return nil, boom },
	}
	l := shuttle.Test(t, newPage(box))

	l.Signal("query", "anything").Change("[data-shuttle-rove-field]")

	if !errors.Is(box.Err(), boom) {
		t.Errorf("Err() = %v, want the search failure", box.Err())
	}
	l.Assert().
		Count("[data-shuttle-rove-item]", 0).
		TextContains("[data-ui=combobox-empty]", "Search failed")
}

// TestComboboxLimit, for a Search that does not cap its own results.
func TestComboboxLimit(t *testing.T) {
	box := &live.Combobox{Search: findPeople, Limit: 1}
	l := shuttle.Test(t, newPage(box))

	l.Signal("query", "").Change("[data-shuttle-rove-field]")
	l.Assert().Count("[data-shuttle-rove-item]", 1)
}

// TestComboboxDisabledRowCannotBeChosen.
func TestComboboxDisabledRowCannotBeChosen(t *testing.T) {
	box := &live.Combobox{
		Search: func(context.Context, string) ([]live.Choice, error) {
			return []live.Choice{{Value: "x", Label: "Unavailable", Disabled: true}}, nil
		},
	}
	l := shuttle.Test(t, newPage(box))
	l.Signal("query", "x").Change("[data-shuttle-rove-field]")

	l.Assert().Count("[data-shuttle-rove-item][disabled]", 1)
	if strings.Contains(l.HTML(), "data-on:click") {
		t.Error("a disabled row still carries a click handler")
	}
}

// TestComboboxWithoutASearch renders rather than panicking, so a
// half-configured component fails visibly instead of taking the page down.
func TestComboboxWithoutASearch(t *testing.T) {
	l := shuttle.Test(t, newPage(&live.Combobox{}))

	l.Signal("query", "anything").Change("[data-shuttle-rove-field]")
	l.Assert().Count("[data-shuttle-rove-item]", 0)
}

// TestComboboxEmptyMessage.
func TestComboboxEmptyMessage(t *testing.T) {
	box := &live.Combobox{Search: findPeople, Empty: "Nobody by that name."}
	l := shuttle.Test(t, newPage(box))

	l.Signal("query", "zzz").Change("[data-shuttle-rove-field]")
	l.Assert().TextContains("[data-ui=combobox-empty]", "Nobody by that name.")
}

// TestComboboxOpenValueIsACounter, not a boolean, and that is the whole
// light-dismiss story: the shim only acts on the attribute when its value
// changes, so Escape's client-side close survives every later patch - a
// stale "true" re-applied would pop the panel back open - while each new
// search mints a new value and reopens it.
func TestComboboxOpenValueIsACounter(t *testing.T) {
	box := &live.Combobox{Search: findPeople}
	l := shuttle.Test(t, newPage(box))

	l.Assert().Attr("[data-shuttle-open]", "data-shuttle-open", "false")

	l.Signal("query", "ada").Change("[data-shuttle-rove-field]")
	l.Assert().Attr("[data-shuttle-open]", "data-shuttle-open", "1")

	l.Signal("query", "grace").Change("[data-shuttle-rove-field]")
	l.Assert().Attr("[data-shuttle-open]", "data-shuttle-open", "2")

	l.Click("[data-shuttle-rove-item]")
	l.Assert().Attr("[data-shuttle-open]", "data-shuttle-open", "false")
}

// TestComboboxWithoutASearchStaysShut. Announcing "No matches." for a
// search that never ran would be the component lying about itself.
func TestComboboxWithoutASearchStaysShut(t *testing.T) {
	l := shuttle.Test(t, newPage(&live.Combobox{}))

	l.Signal("query", "anything").Change("[data-shuttle-rove-field]")
	l.Assert().Attr("[data-shuttle-open]", "data-shuttle-open", "false")
}

// TestComboboxSelectPresetsTheField: editing an existing record is a
// combobox that starts with a value, and the field's signal declaration
// seeds it so the first render already shows it.
func TestComboboxSelectPresetsTheField(t *testing.T) {
	box := &live.Combobox{Search: findPeople}
	box.Select(&live.Choice{Value: "Grace Hopper", Label: "Grace Hopper"})
	l := shuttle.Test(t, newPage(box))

	if got := box.Selected(); got == nil || got.Value != "Grace Hopper" {
		t.Fatalf("Selected() = %v, want the preset", got)
	}
	l.Assert().Contains(`data-signals:c.1.query__ifmissing="&#34;Grace Hopper&#34;"`)

	box.Select(nil)
	if box.Selected() != nil {
		t.Error("Select(nil) did not clear the selection")
	}
}
