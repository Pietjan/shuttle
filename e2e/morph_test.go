//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/playwright-community/playwright-go"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/input"
	"github.com/pietjan/loom/textarea"
	"github.com/pietjan/shuttle"
)

// typing is a component that re-renders while someone is typing into it,
// which is the situation the morph traps live in.
type typing struct {
	shuttle.Base
	Ticks int
	Value bool // render value= on the input
}

func (c *typing) Signals() map[string]any { return map[string]any{"draft": ""} }

func (c *typing) Mount(context.Context, shuttle.Params) error {
	// A re-render nobody asked for, which is the point: pub/sub and timers
	// redraw a component while its user is mid-sentence.
	return c.Every(300*time.Millisecond, func(ctx context.Context) error {
		c.Ticks++
		return nil
	})
}

func (c *typing) Render(ctx context.Context) templ.Component {
	options := []input.Option{
		shuttle.ID(ctx, input.ID, "draft"),
		shuttle.Bind(ctx, input.Attr, "draft"),
		input.Placeholder("type here"),
	}
	if c.Value {
		// The fix under test: the morph compares the value attribute, not
		// the property.
		options = append(options, input.Value(""))
	}

	box := input.New(options...)
	bump := button.New(shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
		c.Ticks++
		return nil
	}))
	return seq(box, with(bump, text("bump")))
}

// TestMorphKeepsWhatWasTyped covers what only a browser can see: whether a
// re-render underneath someone's typing keeps it. Every server-side
// assertion passes either way, because the markup is identical.
func TestMorphKeepsWhatWasTyped(t *testing.T) {
	p := open(t, func() shuttle.Component { return &typing{Value: true} })

	box := p.Locator(`[data-ui="input"]`)
	if err := box.Fill("half a sentence"); err != nil {
		t.Fatalf("fill: %v", err)
	}

	// Let the timer re-render the component underneath the typing.
	time.Sleep(700 * time.Millisecond)

	eventually(t, "the typed value to survive a re-render", func() bool {
		v, _ := box.InputValue()
		return v == "half a sentence"
	})
}

// TestMorphOverwritesWhenTheServerChangesTheValue pins down what the value
// attribute does.
//
// Omitting value= does not wipe anything: the morph only writes attributes
// that differ, and an attribute the server never renders never differs. What
// overwrites the typing is the server rendering a *different* value - then
// the attribute changes, the morph writes it, and the property follows.
func TestMorphOverwritesWhenTheServerChangesTheValue(t *testing.T) {
	cmp := &server{Value: "first"}
	p := open(t, func() shuttle.Component { return cmp })

	box := p.Locator(`[data-ui="input"]`)
	if err := box.Fill("what the user typed"); err != nil {
		t.Fatalf("fill: %v", err)
	}

	// A re-render that does not change the attribute leaves the typing be.
	if err := p.Locator(`[data-shuttle-role="touch"]`).Click(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := expect(p.Locator(`[data-shuttle-renders="2"]`)).ToBeAttached(); err != nil {
		t.Fatalf("waiting for `[data-shuttle-renders=\"2\"]`: %v", err)
	}
	if v, _ := box.InputValue(); v != "what the user typed" {
		t.Errorf("a re-render with an unchanged value= took the typing: %q", v)
	}

	// One that does change it wins, because the attribute is what the morph
	// compares.
	if err := p.Locator(`[data-shuttle-role="change"]`).Click(); err != nil {
		t.Fatalf("change: %v", err)
	}
	eventually(t, "the server's new value to land in the input", func() bool {
		v, _ := box.InputValue()
		return v == "second"
	})
}

// TestActionRunsOnTheServerAndComesBackDownTheStream: the 204-plus-patch
// shape, seen from the client.
func TestActionRunsOnTheServer(t *testing.T) {
	cmp := &typing{Value: true}
	p := open(t, func() shuttle.Component { return cmp })

	if err := p.Locator(`[data-ui="button"]`).Click(); err != nil {
		t.Fatalf("click: %v", err)
	}
	eventually(t, "the action to have run", func() bool { return cmp.Ticks > 0 })
}

// server renders whatever value it was last told to, and counts its own
// renders so a test can wait for one.
type server struct {
	shuttle.Base
	Value   string
	Renders int
}

func (c *server) Render(ctx context.Context) templ.Component {
	c.Renders++

	box := input.New(
		shuttle.ID(ctx, input.ID, "field"),
		input.Value(c.Value),
	)
	touch := button.New(
		button.Attr("data-shuttle-role", "touch"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error { return nil }),
	)
	change := button.New(
		button.Attr("data-shuttle-role", "change"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Value = "second"
			return nil
		}),
	)

	renders := c.Renders
	return seq(box,
		with(touch, text("touch")),
		with(change, text("change")),
		templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<p data-shuttle-renders="%d"></p>`, renders)
			return err
		}),
	)
}

// writing is server's textarea counterpart, and the reason it needs one:
// a textarea has no value attribute. loom's textarea.Value becomes the
// element's text child, so neither rule the two tests above pin is even
// expressible here - there is no attribute to render or omit.
//
// What v1.0.2 does instead, read from the bundle rather than assumed:
//
//	else if (r instanceof HTMLTextAreaElement && s instanceof HTMLTextAreaElement) {
//	    let c = s.value;
//	    r.defaultValue !== c && (r.value = c, l = true)
//	}
//
// The guard is the live element's defaultValue - its text child, which
// typing does not touch - against the incoming content, and the write goes
// to .value directly. Assigning the property rather than the text is what
// sidesteps the dirty-value flag, which would otherwise make a typed-in
// textarea permanently deaf to the server.
//
// So the contract a component author sees is the same as input's, reached a
// different way: render the content from committed state and a re-render
// leaves the typing be; change it and the server wins.
type writing struct {
	shuttle.Base
	Value   string
	Renders int
}

func (c *writing) Render(ctx context.Context) templ.Component {
	c.Renders++

	box := textarea.New(
		shuttle.ID(ctx, textarea.ID, "notes"),
		textarea.Value(c.Value),
	)
	touch := button.New(
		button.Attr("data-shuttle-role", "touch"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error { return nil }),
	)
	change := button.New(
		button.Attr("data-shuttle-role", "change"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Value = "second"
			return nil
		}),
	)

	renders := c.Renders
	return seq(box,
		with(touch, text("touch")),
		with(change, text("change")),
		templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<p data-shuttle-renders="%d"></p>`, renders)
			return err
		}),
	)
}

// rendered waits for the component to have rendered n times, so a test can
// say "after the re-render" without sleeping through one.
func rendered(t *testing.T, p playwright.Page, n int) {
	t.Helper()
	sel := fmt.Sprintf(`[data-shuttle-renders="%d"]`, n)
	if err := expect(p.Locator(sel)).ToBeAttached(); err != nil {
		t.Fatalf("waiting for %s: %v", sel, err)
	}
}

// TestMorphKeepsWhatWasTypedInATextarea is the textarea half of the rule the
// input tests pin. A component rendering committed state that has not moved
// sends the same content every time, the defaultValue guard matches, and
// nothing is written.
func TestMorphKeepsWhatWasTypedInATextarea(t *testing.T) {
	p := open(t, func() shuttle.Component { return &writing{} })

	box := p.Locator(`[data-ui="textarea"]`)
	if err := box.Fill("half a sentence"); err != nil {
		t.Fatalf("fill: %v", err)
	}

	if err := p.Locator(`[data-shuttle-role="touch"]`).Click(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	rendered(t, p, 2)

	if err := expect(box).ToHaveValue("half a sentence"); err != nil {
		t.Errorf("a re-render took the typing out of the textarea: %v", err)
	}
}

// TestMorphOverwritesTheTextareaWhenTheServerChangesIt covers the other
// direction, and then the step that is easy to miss: after the write, the
// morph recurses into the children (r.isEqualNode(s) || nn(r, s)), so the
// text child - and with it defaultValue - converges on what the server sent.
//
// Without that convergence the guard would compare against a stale
// defaultValue forever, and every later re-render would clobber the box
// again. That is the difference between a server that can set a field once
// and a field nobody can type in afterwards.
func TestMorphOverwritesTheTextareaWhenTheServerChangesIt(t *testing.T) {
	p := open(t, func() shuttle.Component { return &writing{Value: "first"} })

	box := p.Locator(`[data-ui="textarea"]`)
	if err := box.Fill("what the user typed"); err != nil {
		t.Fatalf("fill: %v", err)
	}

	// A re-render that does not change the content leaves the typing be.
	if err := p.Locator(`[data-shuttle-role="touch"]`).Click(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	rendered(t, p, 2)
	if v, _ := box.InputValue(); v != "what the user typed" {
		t.Errorf("an unchanged re-render took the typing: %q", v)
	}

	// One that does change it wins, dirty value flag notwithstanding.
	if err := p.Locator(`[data-shuttle-role="change"]`).Click(); err != nil {
		t.Fatalf("change: %v", err)
	}
	if err := expect(box).ToHaveValue("second"); err != nil {
		t.Fatalf("the server's new content never reached the textarea: %v", err)
	}

	// And it settles there: the next edit survives the next re-render, which
	// it could not if defaultValue were still the content from before.
	if err := box.Fill("typed after the change"); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := p.Locator(`[data-shuttle-role="touch"]`).Click(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	rendered(t, p, 4)
	if err := expect(box).ToHaveValue("typed after the change"); err != nil {
		t.Errorf("the textarea kept being overwritten, so defaultValue never caught up: %v", err)
	}
}

var _ = playwright.Float

// churning renders its input with a different id on every render, which is
// what a component gets when it leaves ids to loom's per-render counter
// instead of asking shuttle for a stable one.
type churning struct {
	shuttle.Base
	Renders int
	Stable  bool
}

func (c *churning) Mount(context.Context, shuttle.Params) error {
	return c.Every(300*time.Millisecond, func(context.Context) error { return nil })
}

func (c *churning) Render(ctx context.Context) templ.Component {
	c.Renders++

	id := shuttle.ID(ctx, input.ID, "field")
	if !c.Stable {
		// A different element every time, as far as the morph can tell.
		id = input.ID(fmt.Sprintf("field-%d", c.Renders))
	}
	return input.New(id, input.Placeholder("type here"))
}

// TestMorphNeedsAStableID pins the other half of the boundary, and the one
// the "deterministic ids" decision exists for: the value attribute decides
// what a morph *writes*, but the id decides whether the element is morphed
// at all. Change it every render and the input is replaced - taking the
// typing, and in a real page the focus and the caret with it.
func TestMorphNeedsAStableID(t *testing.T) {
	t.Run("stable", func(t *testing.T) {
		p := open(t, func() shuttle.Component { return &churning{Stable: true} })

		box := p.Locator(`[data-ui="input"]`)
		if err := box.Fill("still here"); err != nil {
			t.Fatalf("fill: %v", err)
		}
		time.Sleep(700 * time.Millisecond) // two re-renders

		if err := expect(box).ToHaveValue("still here"); err != nil {
			t.Errorf("a stable id lost the typing: %v", err)
		}
	})

	t.Run("churning", func(t *testing.T) {
		p := open(t, func() shuttle.Component { return &churning{Stable: false} })

		box := p.Locator(`[data-ui="input"]`)
		if err := box.Fill("gone in a moment"); err != nil {
			t.Fatalf("fill: %v", err)
		}
		time.Sleep(700 * time.Millisecond)

		// Replaced, not morphed: the element the typing was in is gone.
		if err := expect(box).ToHaveValue(""); err != nil {
			t.Errorf("a churning id kept the typing, so the morph matched the input some other way and the naming rules rest on something else: %v", err)
		}
	})
}
