// Command templ is the counter's shape authored the way an application
// will actually write it: the markup lives in todos.templ and is compiled
// by `templ generate`, with loom components and shuttle bindings written
// inline in the template. The other examples assemble their markup in Go
// because each is one file a source panel can show whole; an app with real
// pages wants templates.
//
//	go run ./example/templ
//
// After editing todos.templ, regenerate with `make generate`. The
// generated todos_templ.go is checked in - unlike loom's site, these
// examples are part of the published module, so they must build for anyone
// who `go get`s it, without the templ CLI.
package main

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/checkbox"
	"github.com/pietjan/shuttle"
)

// Todos is a todo list: a draft the client types as a signal, and items
// committed on the server. All the state and behaviour is ordinary Go in
// this file; todos.templ is only the markup.
type Todos struct {
	shuttle.Base
	Items []Item
	next  int
}

// Item is one todo. The ID is what per-row actions close over, and what
// keeps each row's element id stable while rows around it come and go.
type Item struct {
	ID   int
	Text string
	Done bool
}

func (t *Todos) Signals() map[string]any { return map[string]any{"draft": ""} }

func (t *Todos) Render(context.Context) templ.Component { return t.page() }

func (t *Todos) add(ctx context.Context) error {
	var f struct {
		Draft string `json:"draft"`
	}
	if err := shuttle.DecodeSignals(ctx, &f); err != nil {
		return err
	}
	text := strings.TrimSpace(f.Draft)
	if text == "" {
		return nil
	}
	t.next++
	t.Items = append(t.Items, Item{ID: t.next, Text: text})
	// The draft lives on the client, so clearing the box is an explicit,
	// targeted signal write - the same way the kit's combobox writes a
	// chosen label into its field.
	return t.PatchSignal("draft", "")
}

// toggle and remove return closures over an item's id, called from the
// template's loop: a per-row action needs no name and no argument encoding,
// because the render that produced the button captured which row it was.

func (t *Todos) toggle(id int) shuttle.Action {
	return func(context.Context) error {
		for i := range t.Items {
			if t.Items[i].ID == id {
				t.Items[i].Done = !t.Items[i].Done
			}
		}
		return nil
	}
}

func (t *Todos) remove(id int) shuttle.Action {
	return func(context.Context) error {
		t.Items = slices.DeleteFunc(t.Items, func(it Item) bool { return it.ID == id })
		return nil
	}
}

// checkedIf is the conditional-option shape: loom options are values, so
// "an option or nothing" is a helper returning a no-op, which keeps the
// template a single @checkbox.New call.
func checkedIf(done bool) checkbox.Option {
	if done {
		return checkbox.Checked()
	}
	return func(*checkbox.Config) {}
}

func main() {
	h := shuttle.New(func() shuttle.Component { return &Todos{} })
	h.Title = "todos"

	log.Print("listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe("localhost:8080", h))
}
