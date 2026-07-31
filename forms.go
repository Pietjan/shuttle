package shuttle

import (
	"context"
	"sort"
	"strconv"
	"time"
)

// Forms split into two bindings, the way LiveView splits phx-change from
// phx-submit: validate continuously without committing, commit on submit.
// The split is what lets a form tell the user their email is malformed
// while they type without also trying to save it.

// OnChange binds a control's edits to a server-side closure, for the
// validate-as-you-type half of a form.
//
// It listens on input rather than change - change waits for blur, which is
// too late to be useful - so it fires per keystroke and debounce is not
// optional. A debounce of 0 sends one request per character.
//
//	input.New(
//	    shuttle.Bind(ctx, input.Attr, "email"),
//	    shuttle.OnChange(ctx, input.Attr, 300*time.Millisecond, f.validate),
//	)
func OnChange[O ~func(T), T any](ctx context.Context, attr Attrs[O], debounce time.Duration, fn Action, extras ...Extra) O {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return func(T) {}
	}

	event := "input"
	if debounce > 0 {
		// Milliseconds always, rather than Duration.String, which would
		// produce "1.5s" for some values and "300ms" for others.
		event += "__debounce." + strconv.FormatInt(debounce.Milliseconds(), 10) + "ms"
	}
	return bind(attr, sc.pairs(pair{"data-on:" + event, sc.actionExpr(sc.register(fn))}, extras)...)
}

// Validation collects a form's per-field messages. The zero value is not
// usable; start from [Validate].
//
// It pairs with Loom's field package without any adapter, because
// field.Error is documented as a no-op on the empty string precisely so
// results can be passed through unconditionally:
//
//	field.Root(field.Error(v.For("email")))
type Validation map[string]string

// Validate returns an empty Validation ready to collect messages.
func Validate() Validation { return Validation{} }

// Add records a message for a field. The first message for a field wins, so
// a rule chain reports the most specific failure rather than the last one
// checked.
func (v Validation) Add(field, msg string) {
	if _, seen := v[field]; !seen {
		v[field] = msg
	}
}

// Require records msg for field when value is empty. Sugar for the rule
// every form has.
func (v Validation) Require(field, value, msg string) {
	if value == "" {
		v.Add(field, msg)
	}
}

// For returns a field's message, or "" when it is valid - which is exactly
// what field.Error wants.
func (v Validation) For(field string) string { return v[field] }

// Invalid reports whether a field failed.
func (v Validation) Invalid(field string) bool { return v[field] != "" }

// OK reports whether every field passed.
func (v Validation) OK() bool { return len(v) == 0 }

// Fields returns the failing field names, sorted.
func (v Validation) Fields() []string {
	names := make([]string, 0, len(v))
	for f := range v {
		names = append(names, f)
	}
	sort.Strings(names)
	return names
}
