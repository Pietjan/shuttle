package main

import (
	"testing"

	"github.com/pietjan/shuttle"
)

// The template is markup like any other: the kit drives it through the
// session, so what is asserted here is what the browser would hold - ids
// from shuttle.ID and ElementID included, which is what makes the rows
// addressable at all.
func TestTheTemplateDrivesTheList(t *testing.T) {
	l := shuttle.Test(t, &Todos{})

	l.Signal("draft", "  write the example  ").Click("button")
	l.Assert().
		Count("li", 1).
		TextContains("#shuttle-c-item-1", "write the example").
		NoDuplicateIDs()

	l.Click("#shuttle-c-done-1")
	l.Assert().Exists("#shuttle-c-item-1 s")

	l.Click("#shuttle-c-item-1 button")
	l.Assert().Count("li", 0)

	// A blank draft commits nothing.
	l.Signal("draft", "   ").Click("button")
	l.Assert().Count("li", 0)
}
