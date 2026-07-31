package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/badge"
	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/icon"
	"github.com/pietjan/loom/input"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

const lobby = "lobby"

// Room is the payoff for holding state on the server: the stream is already
// open, so a message published anywhere reaches every connected page
// without the client asking for it.
//
// Open this example in two tabs to see it.
type Room struct {
	shuttle.Base
	Heard []string
}

func (r *Room) Signals() map[string]any { return map[string]any{"message": ""} }

// Mount joins the topic, which both subscribes this page and puts it on the
// roster. Presence events arrive through HandleInfo like any other message.
func (r *Room) Mount(ctx context.Context, _ shuttle.Params) error {
	return r.Join(ctx, lobby, "a guest")
}

func (r *Room) HandleInfo(_ context.Context, msg any) error {
	switch m := msg.(type) {
	case shuttle.PresenceEvent:
		verb := "left"
		if m.Joined {
			verb = "arrived"
		}
		r.Heard = append(r.Heard, fmt.Sprintf("%v %s", m.Member.Meta, verb))
	default:
		r.Heard = append(r.Heard, fmt.Sprint(msg))
	}
	if len(r.Heard) > 8 {
		r.Heard = r.Heard[len(r.Heard)-8:]
	}
	return nil
}

func (r *Room) Render(ctx context.Context) templ.Component {
	box := input.New(
		shuttle.ID(ctx, input.ID, "message"),
		shuttle.Bind(ctx, input.Attr, "message"),
		input.Placeholder("Say something…"),
	)
	send := button.New(button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			var f struct {
				Message string `json:"message"`
			}
			if err := shuttle.DecodeSignals(actx, &f); err != nil {
				return err
			}
			if strings.TrimSpace(f.Message) == "" {
				return nil
			}
			// Publish reaches every subscriber, this page included.
			return r.Publish(actx, lobby, "someone said "+strconv.Quote(f.Message))
		}))

	here := len(r.Presence(lobby))

	heard := make([]templ.Component, 0, len(r.Heard))
	for _, line := range r.Heard {
		heard = append(heard, inside(text.New(text.Subtle, text.Small), label(line)))
	}

	return el(`<div class="grid gap-2">`, `</div>`,
		el(`<div class="grid grid-flow-col items-center justify-start gap-2">`, `</div>`,
			box,
			inside(send, icon.New(icon.PaperPlaneTilt), label("Send")),
			// Presence is a live count, which is what a badge is for.
			inside(badge.New(badge.Green, badge.Pill()),
				label(fmt.Sprintf("%d connected", here))),
		),
		el(`<div class="grid gap-2">`, `</div>`, heard...),
	)
}
