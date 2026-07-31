package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/badge"
	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/icon"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// Board renders a keyed child per row. Each child has its own state, its
// own action table and its own morph target, so ticking one row re-renders
// that row and nothing else - which is the whole reason to nest.
//
// The key is the identity rule: the same key keeps the same instance and
// its state across the parent's re-renders, and a key the parent stops
// rendering is unmounted.
type Board struct {
	shuttle.Base
	Rows   []string
	Picked string
	next   int
}

// HandleEvent receives what the rows emit. A child talks to its parent this
// way rather than either holding a reference to the other.
func (b *Board) HandleEvent(_ context.Context, name string, payload any) error {
	b.Picked = fmt.Sprintf("%s: %v", name, payload)
	return nil
}

func (b *Board) Mount(context.Context, shuttle.Params) error {
	b.Rows = []string{"alpha", "beta", "gamma"}
	b.next = len(b.Rows)
	return nil
}

func (b *Board) Render(ctx context.Context) templ.Component {
	add := button.New(button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			b.next++
			b.Rows = append(b.Rows, fmt.Sprintf("row %d", b.next))
			return nil
		}))

	rows := make([]templ.Component, 0, len(b.Rows))
	for _, label := range b.Rows {
		name := label // captured per row, which is what closures buy
		rows = append(rows, shuttle.Child(ctx, name, func() shuttle.Component {
			return &Tally{Label: name}
		}))
	}

	return group(
		inside(add, icon.New(icon.Plus), label("Add a row")),
		el(`<div class="grid gap-2">`, `</div>`, rows...),
		inside(text.New(text.Subtle, text.Small), label("Last emitted: "+orNothing(b.Picked))),
	)
}

// Tally is the child. Its Ticks belong to it, survive the parent
// re-rendering around it, and are lost when the parent stops rendering its
// key.
type Tally struct {
	shuttle.Base
	Label string
	Ticks int
}

func (t *Tally) Render(ctx context.Context) templ.Component {
	tick := button.New(
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			t.Ticks++
			return nil
		}))
	// Emit reaches the nearest ancestor implementing Receiver, and
	// re-renders it. Without it, this child's action would re-render only
	// this child - which is exactly what the tick button does.
	pick := button.New(
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			return t.Emit(actx, "picked", t.Label)
		}))

	return el(
		`<div class="grid grid-flow-col items-center justify-start gap-2">`,
		`</div>`,
		inside(text.Strong(), label(t.Label)),
		inside(badge.New(badge.Zinc, badge.Pill()), label(strconv.Itoa(t.Ticks))),
		inside(tick, label("tick")),
		inside(pick, label("emit")),
	)
}
