package shuttle

import "strings"

// Assert returns the assertions for the component's current markup.
//
//	live.Assert().Text("#count", "1").Count("li", 3)
func (l *Live) Assert() *Assert { return &Assert{l: l} }

// Assert checks a component's rendered markup. Every method returns the
// same value, so checks chain, and each reports through the test rather
// than stopping it - one failing assertion should not hide the next.
type Assert struct{ l *Live }

// Text checks the collapsed text of the first element matching sel.
func (a *Assert) Text(sel, want string) *Assert {
	a.l.tb.Helper()

	n := a.l.first(sel)
	if n == nil {
		a.l.tb.Errorf("shuttle: no element matches %q in:\n%s", sel, a.l.markup)
		return a
	}
	if got := textOf(n); got != want {
		a.l.tb.Errorf("shuttle: text of %q = %q, want %q", sel, got, want)
	}
	return a
}

// TextContains checks that the first element matching sel contains want.
func (a *Assert) TextContains(sel, want string) *Assert {
	a.l.tb.Helper()

	n := a.l.first(sel)
	if n == nil {
		a.l.tb.Errorf("shuttle: no element matches %q in:\n%s", sel, a.l.markup)
		return a
	}
	if got := textOf(n); !strings.Contains(got, want) {
		a.l.tb.Errorf("shuttle: text of %q = %q, want it to contain %q", sel, got, want)
	}
	return a
}

// Attr checks an attribute on the first element matching sel.
func (a *Assert) Attr(sel, key, want string) *Assert {
	a.l.tb.Helper()

	n := a.l.first(sel)
	if n == nil {
		a.l.tb.Errorf("shuttle: no element matches %q in:\n%s", sel, a.l.markup)
		return a
	}
	if got := attr(n, key); got != want {
		a.l.tb.Errorf("shuttle: %q's %s = %q, want %q", sel, key, got, want)
	}
	return a
}

// Count checks how many elements match sel.
func (a *Assert) Count(sel string, want int) *Assert {
	a.l.tb.Helper()

	found, err := selectAll(a.l.markup, sel)
	if err != nil {
		a.l.tb.Errorf("shuttle: parsing markup: %v", err)
		return a
	}
	if len(found) != want {
		a.l.tb.Errorf("shuttle: %d elements match %q, want %d", len(found), sel, want)
	}
	return a
}

// Exists checks that at least one element matches sel.
func (a *Assert) Exists(sel string) *Assert {
	a.l.tb.Helper()

	if a.l.first(sel) == nil {
		a.l.tb.Errorf("shuttle: nothing matches %q in:\n%s", sel, a.l.markup)
	}
	return a
}

// Missing checks that nothing matches sel.
func (a *Assert) Missing(sel string) *Assert {
	a.l.tb.Helper()

	if n := a.l.first(sel); n != nil {
		a.l.tb.Errorf("shuttle: %q should not be rendered, but is", sel)
	}
	return a
}

// Contains checks the raw markup, for the cases a selector cannot reach -
// an attribute value, a signal declaration.
func (a *Assert) Contains(want string) *Assert {
	a.l.tb.Helper()

	if !strings.Contains(a.l.markup, want) {
		a.l.tb.Errorf("shuttle: markup does not contain %q:\n%s", want, a.l.markup)
	}
	return a
}

// NoDuplicateIDs checks the thing Datastar reports nothing about: a
// duplicate id is excluded from its persistent-id set, which silently drops
// that subtree to soft matching, so focus and scroll start going missing
// under load with no error anywhere.
//
// Worth calling in every component's test.
func (a *Assert) NoDuplicateIDs() *Assert {
	a.l.tb.Helper()

	if dupes := DuplicateIDs(a.l.markup); dupes != nil {
		a.l.tb.Errorf("shuttle: duplicate element ids %v will degrade morphing:\n%s",
			dupes, a.l.markup)
	}
	return a
}

// URL checks where the component has driven the address bar.
func (a *Assert) URL(want string) *Assert {
	a.l.tb.Helper()

	if got := a.l.sess.currentURL(); got != want {
		a.l.tb.Errorf("shuttle: url = %q, want %q", got, want)
	}
	return a
}
