//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/mxschmitt/playwright-go"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

// people is the set the combobox searches. Small enough to reason about,
// which is all a browser test needs from its data.
var people = []live.Choice{
	{Value: "ada", Label: "Ada Lovelace"},
	{Value: "alan", Label: "Alan Turing"},
	{Value: "grace", Label: "Grace Hopper"},
}

func search(_ context.Context, q string) ([]live.Choice, error) {
	if q == "" {
		return people, nil
	}
	var out []live.Choice
	for _, c := range people {
		if len(q) <= len(c.Label) && contains(c.Label, q) {
			out = append(out, c)
		}
	}
	return out, nil
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// TestComboboxPanelIsAPopover covers the four behaviours the popover was
// chosen for. Each was verified by hand once; a test is what keeps them
// verified.
func TestComboboxPanelIsAPopover(t *testing.T) {
	p := open(t, func() shuttle.Component {
		return &live.Combobox{Label: "Person", Placeholder: "Find…", Search: search}
	})

	field := p.Locator(`[data-ui="combobox-input"]`)
	panel := p.Locator(`[data-ui="combobox-list"]`)

	open := func() bool {
		n, err := p.Locator(`[data-ui="combobox-list"]:popover-open`).Count()
		return err == nil && n == 1
	}

	if open() {
		t.Fatal("the panel starts open")
	}

	// Typing opens it: the server answers with results and says so, and the
	// shim shows the panel to match.
	if err := field.Fill("a"); err != nil {
		t.Fatalf("fill: %v", err)
	}
	eventually(t, "the panel to open on typing", open)

	// It floats rather than pushing the page around, which is the whole
	// reason for the popover.
	box, err := panel.BoundingBox()
	if err != nil {
		t.Fatalf("panel box: %v", err)
	}
	fieldBox, err := field.BoundingBox()
	if err != nil {
		t.Fatalf("field box: %v", err)
	}
	if box.Y < fieldBox.Y+fieldBox.Height-2 {
		t.Errorf("panel is not below the field: panel y=%v, field bottom=%v",
			box.Y, fieldBox.Y+fieldBox.Height)
	}

	// A click outside closes it, and that is the platform's light dismiss
	// rather than anything shuttle wrote.
	if err := p.Mouse().Click(5, 5); err != nil {
		t.Fatalf("click away: %v", err)
	}
	eventually(t, "an outside click to dismiss the panel", func() bool { return !open() })

	// Escape closes it too - the roving handler preventDefaults Escape, so
	// the shim has to close the panel itself.
	if err := field.Fill("al"); err != nil {
		t.Fatalf("refill: %v", err)
	}
	eventually(t, "the panel to reopen", open)
	if err := field.Press("Escape"); err != nil {
		t.Fatalf("escape: %v", err)
	}
	eventually(t, "Escape to dismiss the panel", func() bool { return !open() })
}

// TestComboboxChoosingClosesIt: after a choice there is nothing left to
// choose, and the server says so with data-shuttle-open.
func TestComboboxChoosingClosesIt(t *testing.T) {
	var chosen string
	p := open(t, func() shuttle.Component {
		return &live.Combobox{
			Label: "Person", Search: search,
			OnSelect: func(_ context.Context, c live.Choice) error {
				chosen = c.Value
				return nil
			},
		}
	})

	if err := p.Locator(`[data-ui="combobox-input"]`).Fill("ada"); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := expect(p.Locator(`[data-ui="combobox-item"]`)).ToBeAttached(); err != nil {
		t.Fatalf("waiting for `[data-ui=\"combobox-item\"]`: %v", err)
	}

	if err := p.Locator(`[data-ui="combobox-item"]`).First().Click(); err != nil {
		t.Fatalf("choose: %v", err)
	}
	eventually(t, "the choice to reach the server", func() bool { return chosen == "ada" })
	eventually(t, "the panel to close after a choice", func() bool {
		n, err := p.Locator(`[data-ui="combobox-list"]:popover-open`).Count()
		return err == nil && n == 0
	})
}

// TestComboboxArrowKeysMoveRealFocus. The kit moves focus rather than
// painting listbox roles onto buttons, which is a claim about what the page
// does, not about what it renders: Enter stays native because the thing
// focused is a real button.
func TestComboboxArrowKeysMoveRealFocus(t *testing.T) {
	p := open(t, func() shuttle.Component {
		return &live.Combobox{Label: "Person", Search: search}
	})

	field := p.Locator(`[data-ui="combobox-input"]`)
	if err := field.Fill("a"); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := expect(p.Locator(`[data-ui="combobox-item"]`).First()).ToBeVisible(); err != nil {
		t.Fatalf("no results: %v", err)
	}

	focused := func() string {
		v, err := p.Evaluate(`() => document.activeElement && document.activeElement.textContent.trim()`)
		if err != nil {
			t.Fatalf("activeElement: %v", err)
		}
		s, _ := v.(string)
		return s
	}

	if err := field.Press("ArrowDown"); err != nil {
		t.Fatalf("down: %v", err)
	}
	eventually(t, "focus to move to the first row", func() bool { return focused() == "Ada Lovelace" })

	if err := p.Keyboard().Press("ArrowDown"); err != nil {
		t.Fatalf("down: %v", err)
	}
	eventually(t, "focus to move to the second row", func() bool { return focused() == "Alan Turing" })

	// Up from the first row goes back to the field, not past it.
	if err := p.Keyboard().Press("ArrowUp"); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := p.Keyboard().Press("ArrowUp"); err != nil {
		t.Fatalf("up: %v", err)
	}
	eventually(t, "focus to return to the field", func() bool {
		v, err := p.Evaluate(`() => document.activeElement && document.activeElement.getAttribute('data-ui')`)
		if err != nil {
			return false
		}
		s, _ := v.(string)
		return s == "combobox-input"
	})
}

// logger streams entries into a container it does not hold, and re-renders
// itself on demand.
type logger struct {
	shuttle.Base
	Sent int
}

func (l *logger) add(ctx context.Context) error {
	l.Sent++
	key := strconv.Itoa(l.Sent)
	return l.Stream("log").Append(ctx, key, templ.ComponentFunc(
		func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<li id=%q data-line>line %s</li>`,
				l.Stream("log").ItemID(key), key)
			return err
		}))
}

func (l *logger) Render(ctx context.Context) templ.Component {
	add := button.New(
		button.Attr("data-shuttle-role", "add"),
		shuttle.OnClick(ctx, button.Attr, l.add),
	)
	touch := button.New(
		button.Attr("data-shuttle-role", "touch"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error { return nil }),
	)

	stream := l.Stream("log")
	sent := l.Sent
	return seq(
		with(add, text("add")),
		with(touch, text("touch")),
		templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<p data-sent="%d"></p><ul%s></ul>`, sent, stream.Attrs())
			return err
		}),
	)
}

// TestStreamedItemsSurviveARerender is the property data-ignore-morph
// exists for, and the one that cannot be checked server-side at all: the
// server has forgotten these items, so only the page knows whether they are
// still there.
func TestStreamedItemsSurviveARerender(t *testing.T) {
	p := open(t, func() shuttle.Component { return &logger{} })

	for i := 1; i <= 3; i++ {
		if err := p.Locator(`[data-shuttle-role="add"]`).Click(); err != nil {
			t.Fatalf("add: %v", err)
		}
		if err := expect(p.Locator(fmt.Sprintf(`[data-sent="%d"]`, i))).ToBeAttached(); err != nil {
			t.Fatalf("waiting for the %d%s append to land: %v", i, ordinal(i), err)
		}
	}
	eventually(t, "three streamed lines", func() bool {
		n, err := p.Locator(`[data-line]`).Count()
		return err == nil && n == 3
	})

	// A re-render of the component that owns the container. Without
	// data-ignore-morph this is where the lines disappear.
	if err := p.Locator(`[data-shuttle-role="touch"]`).Click(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	n, err := p.Locator(`[data-line]`).Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("a re-render left %d streamed lines, want 3", n)
	}
}

// ordinal keeps a failure message readable: "the 2nd append".
func ordinal(n int) string {
	switch n {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

var _ = playwright.Float
