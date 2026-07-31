package main

import (
	"context"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/field"
	"github.com/pietjan/loom/input"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// Signup is the change/submit split. OnChange validates as you type without
// committing anything; OnSubmit commits, but only if the same rules pass.
//
// The typing itself lives on the client as a signal, so keystrokes cost
// nothing until the debounce expires.
type Signup struct {
	shuttle.Base
	Email  string // committed
	Saved  bool
	Errors shuttle.Validation
}

func (s *Signup) Signals() map[string]any { return map[string]any{"email": ""} }

func (s *Signup) validate(ctx context.Context) error {
	var f struct {
		Email string `json:"email"`
	}
	if err := shuttle.DecodeSignals(ctx, &f); err != nil {
		return err
	}

	s.Errors = shuttle.Validate()
	s.Errors.Require("email", f.Email, "An email address is required.")
	if f.Email != "" && !strings.Contains(f.Email, "@") {
		s.Errors.Add("email", "That does not look like an email address.")
	}
	return nil
}

func (s *Signup) submit(ctx context.Context) error {
	if err := s.validate(ctx); err != nil {
		return err
	}
	if !s.Errors.OK() {
		s.Saved = false
		return nil
	}

	var f struct {
		Email string `json:"email"`
	}
	if err := shuttle.DecodeSignals(ctx, &f); err != nil {
		return err
	}
	s.Email, s.Saved = f.Email, true
	return nil
}

func (s *Signup) Render(ctx context.Context) templ.Component {
	box := input.New(
		shuttle.ID(ctx, input.ID, "email"),
		shuttle.Bind(ctx, input.Attr, "email"),
		// Rendering value= is not optional: the morph compares the value
		// attribute, so omitting it would clear what was typed.
		input.Value(s.Email),
		input.Placeholder("you@example.com"),
		// OnChange listens on input, not change - change waits for blur,
		// which is too late to be useful - so it fires per keystroke and the
		// debounce is doing real work.
		shuttle.OnChange(ctx, input.Attr, 300*time.Millisecond, s.validate),
	)
	save := button.New(button.Primary,
		shuttle.OnClick(ctx, button.Attr, s.submit))

	// field.Error is a no-op on the empty string, so a validation result
	// passes through unconditionally.
	form := group(
		inside(field.Root(field.Error(s.Errors.For("email"))), box),
		inside(save, label("Sign up")),
	)
	if !s.Saved {
		return form
	}
	return group(form,
		inside(text.New(text.Subtle, text.Small), label("Saved "+s.Email+".")))
}
