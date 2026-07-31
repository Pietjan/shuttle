package shuttle_test

import (
	"context"
	"net/http"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/shuttle"
)

// Counter is the smallest useful component: state as ordinary fields, and
// an action bound as a closure over them.
type Counter struct {
	shuttle.Base
	Count int
}

func (c *Counter) Render(ctx context.Context) templ.Component {
	return button.New(
		button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count++
			return nil
		}),
	)
}

// One instance is created per page load and lives as long as that page
// stays connected.
func ExampleNew() {
	h := shuttle.New(func() shuttle.Component { return &Counter{} })
	h.Title = "counter"

	http.ListenAndServe(":8080", h)
}

// A handler can live anywhere in an app's routes, as long as it is told
// where: the URLs it renders are built from Prefix.
func ExampleHandler_Prefix() {
	h := shuttle.New(func() shuttle.Component { return &Counter{} })
	h.Prefix = "/dashboard"
	h.Head = `<link rel="stylesheet" href="/static/app.css">`

	mux := http.NewServeMux()
	mux.Handle("/dashboard/", h)

	http.ListenAndServe(":8080", mux)
}
