package live

import (
	"context"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/combobox"
	"github.com/pietjan/shuttle"
)

// DefaultDebounce is how long a Combobox waits before asking the server,
// when one is not set. Zero would be a request per keystroke.
const DefaultDebounce = 200 * time.Millisecond

// Choice is one option a Combobox can offer.
type Choice struct {
	Value string
	Label string
	// Disabled renders the row but refuses selection.
	Disabled bool
}

// Combobox is filter-as-you-type over a set the server owns - the case
// Loom's README names as missing, because narrowing a list as someone types
// needs somewhere for the query to go.
//
//	shuttle.Child(ctx, "user", func() shuttle.Component {
//	    return &live.Combobox{
//	        Placeholder: "Find a user…",
//	        Search:      searchUsers,
//	        OnSelect:    func(ctx context.Context, c live.Choice) error { ... },
//	    }
//	})
//
// The query lives on the client, so typing costs nothing until the debounce
// expires. Results are rendered by the server, which means the set can be
// any size and never has to reach the browser.
//
// Rows are buttons and the arrow keys move focus between them, rather than
// listbox roles announcing something the rows do not do. Escape returns to
// the field.
//
// The panel floats: it is Loom's combobox, whose list carries the popover
// attribute, so it overlays the page instead of pushing it and its light
// dismiss is the platform's. Opening is this component's business - the
// shim opens it when results arrive and closes it on a choice - because
// nothing in Loom runs.
type Combobox struct {
	shuttle.Base

	// Search returns the choices matching a query. It runs on the session's
	// goroutine, so it can touch the component's own state, and it is the
	// only thing that decides what a query means - prefix, fuzzy, ranked.
	Search func(ctx context.Context, query string) ([]Choice, error)

	// OnSelect is called when a row is chosen, after Selected is set.
	OnSelect func(ctx context.Context, choice Choice) error

	// Placeholder for the field.
	Placeholder string

	// Label is the control's accessible name. Without one the placeholder
	// stands in, which is better than nothing and worse than a label.
	Label string

	// Empty is shown when a search returns nothing.
	Empty string

	// Debounce is how long to wait after a keystroke. Zero means
	// DefaultDebounce; negative asks on every keystroke, which is rarely
	// what anyone wants.
	Debounce time.Duration

	// Limit caps how many rows are rendered. Zero means no cap - which is
	// only reasonable if Search already limits its own results.
	Limit int

	choices  []Choice
	selected *Choice
	searched bool
	failed   error
}

// Selected returns the chosen option, or nil.
func (c *Combobox) Selected() *Choice { return c.selected }

// Query is the client-side signal holding what has been typed.
func (c *Combobox) Signals() map[string]any {
	return map[string]any{"query": ""}
}

// Choices returns what the last search found.
func (c *Combobox) Choices() []Choice { return c.choices }

// Err returns the last search failure, if any.
func (c *Combobox) Err() error { return c.failed }

func (c *Combobox) debounce() time.Duration {
	switch {
	case c.Debounce < 0:
		return 0
	case c.Debounce == 0:
		return DefaultDebounce
	default:
		return c.Debounce
	}
}

// search runs when the field changes.
func (c *Combobox) search(ctx context.Context) error {
	var f struct {
		Query string `json:"query"`
	}
	if err := shuttle.DecodeSignals(ctx, &f); err != nil {
		return err
	}

	c.searched = true
	c.failed = nil

	if c.Search == nil {
		c.choices = nil
		return nil
	}

	choices, err := c.Search(ctx, f.Query)
	if err != nil {
		// A failed search is the component's problem to show, not the
		// page's to die of.
		c.choices, c.failed = nil, err
		return nil
	}
	if c.Limit > 0 && len(choices) > c.Limit {
		choices = choices[:c.Limit]
	}
	c.choices = choices
	return nil
}

// choose records a selection and tells the caller.
func (c *Combobox) choose(ctx context.Context, choice Choice) error {
	if choice.Disabled {
		return nil
	}
	picked := choice
	c.selected = &picked

	// Nothing left to choose, so the panel closes - which the next render
	// says with data-shuttle-open and the shim carries out.
	c.choices, c.searched = nil, false

	if c.OnSelect != nil {
		return c.OnSelect(ctx, picked)
	}
	return nil
}

func (c *Combobox) Render(ctx context.Context) templ.Component {
	// A stable stem, so a re-render morphs the rows that are already there
	// instead of replacing them and taking focus with them - and so the
	// panel survives, since a popover's open state lives on the element.
	stem := shuttle.ElementID(ctx, "combobox")

	// Open exactly when there is something to show. The shim reads this and
	// calls showPopover/hidePopover, which is the one thing the markup
	// cannot say for itself.
	open := c.searched && (len(c.choices) > 0 || c.failed != nil || c.emptyMessage() != "")

	field := combobox.Input(
		combobox.Placeholder(c.Placeholder),
		combobox.Attr("aria-label", c.label()),
		shuttle.Bind(ctx, combobox.Attr, "query"),
		shuttle.OnChange(ctx, combobox.Attr, c.debounce(), c.search),
		combobox.Attr("data-shuttle-rove-field", ""),
	)
	if open {
		field = combobox.Input(
			combobox.Placeholder(c.Placeholder),
			combobox.Attr("aria-label", c.label()),
			combobox.Expanded(),
			shuttle.Bind(ctx, combobox.Attr, "query"),
			shuttle.OnChange(ctx, combobox.Attr, c.debounce(), c.search),
			combobox.Attr("data-shuttle-rove-field", ""),
		)
	}

	rows := make([]templ.Component, 0, len(c.choices))
	for _, choice := range c.choices {
		opts := []combobox.Option{
			combobox.Value(choice.Value),
			combobox.Attr("data-shuttle-rove-item", ""),
		}
		if c.selected != nil && c.selected.Value == choice.Value {
			opts = append(opts, combobox.Chosen())
		}
		if choice.Disabled {
			opts = append(opts, combobox.Disabled())
		} else {
			// The closure captures this row's choice, which is the whole
			// argument for holding state on the server: no value has to be
			// encoded into the markup and decoded back out of a request.
			opts = append(opts, shuttle.OnClick(ctx, combobox.Attr,
				func(actx context.Context) error { return c.choose(actx, choice) }))
		}
		rows = append(rows, with(combobox.Item(opts...), text(choice.Label)))
	}

	body := make([]templ.Component, 0, len(rows)+1)
	body = append(body, rows...)
	if len(rows) == 0 {
		body = append(body, with(combobox.Empty(), text(c.emptyMessage())))
	}

	return with(
		combobox.Root(
			combobox.Name(stem),
			combobox.Attr("data-shuttle-roving", ""),
			// What the shim acts on. An attribute rather than a signal: it
			// is a fact about this render, not client state.
			combobox.Attr("data-shuttle-open", strconv.FormatBool(open)),
		),
		field,
		with(combobox.List(), body...),
	)
}

// label names the control for assistive tech. A placeholder is not a name,
// and this component has no field wrapper to borrow one from.
func (c *Combobox) label() string {
	if c.Label != "" {
		return c.Label
	}
	if c.Placeholder != "" {
		return c.Placeholder
	}
	return "Search"
}

func (c *Combobox) emptyMessage() string {
	if c.failed != nil {
		return "Search failed."
	}
	if c.Empty != "" {
		return c.Empty
	}
	return "No matches."
}
