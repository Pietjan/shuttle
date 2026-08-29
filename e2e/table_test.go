//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

type person struct{ Name, Role, Team string }

var directory = []person{
	{"Ada Lovelace", "Principal", "core"},
	{"Alan Turing", "Staff", "core"},
	{"Grace Hopper", "Principal", "tools"},
	{"Edsger Dijkstra", "Staff", "tools"},
	{"Barbara Liskov", "Distinguished", "core"},
	{"Donald Knuth", "Staff", "docs"},
	{"Katherine Johnson", "Distinguished", "core"},
	{"Margaret Hamilton", "Principal", "tools"},
	{"Radia Perlman", "Distinguished", "core"},
	{"Frances Allen", "Principal", "tools"},
	{"Jean Bartik", "Senior", "docs"},
	{"Anita Borg", "Staff", "docs"},
}

func load(_ context.Context, q live.Query) (live.Page[person], error) {
	var matched []person
	for _, p := range directory {
		hay := strings.ToLower(p.Name + " " + p.Role + " " + p.Team)
		if q.Filter == "" || strings.Contains(hay, strings.ToLower(q.Filter)) {
			matched = append(matched, p)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		less := matched[i].Name < matched[j].Name
		if q.Sort == "role" {
			less = matched[i].Role < matched[j].Role
		}
		if q.Desc {
			return !less
		}
		return less
	})
	if q.Offset >= len(matched) {
		return live.Page[person]{Total: len(matched)}, nil
	}
	end := min(q.Offset+q.Limit, len(matched))
	return live.Page[person]{Rows: matched[q.Offset:end], Total: len(matched)}, nil
}

func table() shuttle.Component {
	return &live.Table[person]{
		Columns: []live.Column[person]{
			{
				Key: "name", Title: "Name", Sortable: true, Width: "w-52",
				Cell: func(_ context.Context, p person) templ.Component { return live.Text(p.Name) },
			},
			{
				Key: "role", Title: "Role", Sortable: true, Width: "w-36",
				Cell: func(_ context.Context, p person) templ.Component { return live.Text(p.Role) },
			},
			{
				Key: "team", Title: "Team", Width: "w-28",
				Cell: func(_ context.Context, p person) templ.Component { return live.Text(p.Team) },
			},
		},
		Load:       load,
		PageSize:   5,
		Filterable: true,
		Choosable:  true,
		Empty:      "Nobody matches.",
	}
}

// shape is what "jumpy" was: the numbers that must not move while someone
// sorts, pages and types.
type shape struct {
	Width   float64   `json:"width"`
	Height  float64   `json:"height"`
	Columns []float64 `json:"columns"`
	PagerY  float64   `json:"pagerY"`
}

func measure(t *testing.T, p interface {
	Evaluate(string, ...any) (any, error)
},
) shape {
	t.Helper()
	raw, err := p.Evaluate(`() => {
		const w = document.querySelector('[data-ui="table-wrapper"]').getBoundingClientRect();
		const pager = document.querySelector('[data-shuttle-pager-bar]').getBoundingClientRect();
		return {
			width: Math.round(w.width), height: Math.round(w.height),
			columns: [...document.querySelectorAll('thead th')].map(th => Math.round(th.getBoundingClientRect().width)),
			pagerY: Math.round(pager.top),
		};
	}`)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("measure returned %T", raw)
	}
	out := shape{
		Width:  num(t, m["width"]),
		Height: num(t, m["height"]),
		PagerY: num(t, m["pagerY"]),
	}
	cols, ok := m["columns"].([]any)
	if !ok {
		t.Fatalf("columns came back as %T", m["columns"])
	}
	for _, c := range cols {
		out.Columns = append(out.Columns, num(t, c))
	}
	return out
}

// num accepts whatever the protocol decoded a JSON number into: a whole one
// arrives as int, a fractional one as float64.
func num(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		t.Fatalf("expected a number, got %T (%v)", v, v)
		return 0
	}
}

func (s shape) String() string {
	return fmt.Sprintf("%.0fx%.0f cols=%v pager@%.0f", s.Width, s.Height, s.Columns, s.PagerY)
}

func (s shape) equal(o shape) bool {
	if s.Width != o.Width || s.Height != o.Height || s.PagerY != o.PagerY {
		return false
	}
	if len(s.Columns) != len(o.Columns) {
		return false
	}
	for i := range s.Columns {
		if s.Columns[i] != o.Columns[i] {
			return false
		}
	}
	return true
}

// TestTableKeepsItsShapeInThePage is the measurement that made "the table is
// a bit jumpy" actionable: before the fixed layout, the padded pages and the
// empty state that keeps its headings, the width moved by 66px across these
// steps and the pager walked 135px up the screen.
func TestTableKeepsItsShapeInThePage(t *testing.T) {
	needsStylesheet(t)
	p := open(t, table, styled)

	if err := expect(p.Locator(`[data-ui="table-wrapper"]`)).ToBeVisible(); err != nil {
		t.Fatalf("no table: %v", err)
	}
	start := measure(t, p)

	steps := []struct {
		name string
		do   func() error
	}{
		{"sorted by role", func() error { return p.Locator(`[data-shuttle-sort="role"]`).Click() }},
		{"page 2", func() error { return p.Locator(`[data-shuttle-page="2"]`).Click() }},
		{"filtered to a short page", func() error {
			return p.Locator(`[data-shuttle-table-filter]`).Fill("liskov")
		}},
		{"nothing matches", func() error {
			return p.Locator(`[data-shuttle-table-filter]`).Fill("nobody at all")
		}},
	}

	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		// Let the round trip and its morph land before measuring.
		if err := expect(p.Locator(`[data-ui="table-wrapper"]`)).ToBeVisible(); err != nil {
			t.Fatalf("%s: table gone: %v", step.name, err)
		}
		// One settle, then compare - and report both shapes, because "it
		// moved" is not a useful failure for a layout test.
		var got shape
		for i := 0; i < 40; i++ {
			got = measure(t, p)
			if got.equal(start) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !got.equal(start) {
			t.Errorf("%s changed the shape:\n got %s\nwant %s", step.name, got, start)
		}
	}

	// The headings survive an empty result, which is half of why the shape
	// holds.
	if err := expect(p.Locator(`[data-shuttle-sort="name"]`)).ToBeVisible(); err != nil {
		t.Errorf("an empty table lost its headings: %v", err)
	}
	if err := expect(p.Locator(`[data-shuttle-table-empty]`)).ToContainText("Nobody matches."); err != nil {
		t.Errorf("no empty message: %v", err)
	}
}

// TestSortCyclesInThePage: ascending, descending, and back to the order the
// source returns - the third state being the only way back.
func TestSortCyclesInThePage(t *testing.T) {
	p := open(t, table)

	header := p.Locator(`th:has([data-shuttle-sort="role"])`)
	if err := expect(header).ToHaveAttribute("aria-sort", "none"); err != nil {
		t.Fatalf("unsorted column does not say so: %v", err)
	}

	for _, want := range []string{"ascending", "descending", "none"} {
		if err := p.Locator(`[data-shuttle-sort="role"]`).Click(); err != nil {
			t.Fatalf("sort: %v", err)
		}
		if err := expect(header).ToHaveAttribute("aria-sort", want); err != nil {
			t.Errorf("after a click the column is not %s: %v", want, err)
		}
	}
}

// TestColumnPickerStaysOpen is what dropdown.Name bought: the menu is a
// popover, whose open state lives on the element, so a re-render that
// replaced it would close the menu after every single toggle.
func TestColumnPickerStaysOpen(t *testing.T) {
	p := open(t, table)

	if err := p.Locator(`[data-shuttle-columns]`).Click(); err != nil {
		t.Fatalf("open the picker: %v", err)
	}
	menu := p.Locator(`[data-ui="dropdown-menu"]:popover-open`)
	if err := expect(menu).ToBeVisible(); err != nil {
		t.Fatalf("the picker did not open: %v", err)
	}

	for _, key := range []string{"team", "role"} {
		if err := p.Locator(`[data-shuttle-column="` + key + `"]`).Click(); err != nil {
			t.Fatalf("toggle %s: %v", key, err)
		}
		if err := expect(p.Locator(`[data-shuttle-sort="` + key + `"]`)).ToHaveCount(0); err != nil {
			t.Errorf("%s did not disappear from the table: %v", key, err)
		}
		if err := expect(menu).ToBeVisible(); err != nil {
			t.Fatalf("the menu closed after toggling %s, so a second column costs a second trip: %v", key, err)
		}
	}

	// The last one refuses: a table with no columns has no picker to get
	// them back from.
	if err := expect(p.Locator(`[data-shuttle-column="name"]`)).ToHaveAttribute("aria-disabled", "true"); err != nil {
		t.Errorf("the last visible column is not protected: %v", err)
	}
}
