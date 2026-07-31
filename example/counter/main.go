// Command counter is the smallest thing shuttle can serve: one component,
// one action, no configuration.
//
//	go run ./example/counter
//
// For the rest - forms, nesting, pub/sub, uploads, the live component kit -
// see ./example/site.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/shuttle"
)

// Counter holds its state as an ordinary struct field. Nothing here is
// serialised to the client, and nothing comes back from it.
type Counter struct {
	shuttle.Base
	Count int
}

func (c *Counter) Render(ctx context.Context) templ.Component {
	// The action is a closure over the component, so there is no action
	// name to keep in sync and no arguments to encode.
	inc := button.New(
		button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count++
			return nil
		}),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := fmt.Fprintf(w, `<h1>Count: %d</h1>`, c.Count); err != nil {
			return err
		}
		return inc.Render(templ.WithChildren(ctx, templ.Raw("+1")), w)
	})
}

func main() {
	h := shuttle.New(func() shuttle.Component { return &Counter{} })
	h.Title = "counter"

	log.Print("listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe("localhost:8080", h))
}
