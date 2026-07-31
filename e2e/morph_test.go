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

// TestMorphKeepsWhatWasTyped is the trap that has cost the most time here:
// a re-render that omits value= wipes the input, and only in a browser -
// every server-side assertion passes, because the markup is identical.
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
// attribute actually does, which is not what the note in CLAUDE.md said.
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
