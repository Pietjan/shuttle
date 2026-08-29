package main

import (
	"context"
	"sort"
	"strings"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/badge"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

// The set both kit examples work over. It stays here, on the server: the
// point of filtering and paginating in a stateful layer is that the browser
// never receives the whole thing.
type person struct {
	Name   string
	Team   string
	Role   string
	Place  string
	Joined string // ISO, so sorting it as a string sorts it as a date
}

var directory = []person{
	{"Ada Lovelace", "core", "Principal", "London", "2019-04-02"},
	{"Alan Turing", "core", "Staff", "Wilmslow", "2020-06-23"},
	{"Grace Hopper", "tools", "Principal", "New York", "2018-12-09"},
	{"Edsger Dijkstra", "tools", "Staff", "Rotterdam", "2021-05-11"},
	{"Barbara Liskov", "core", "Distinguished", "Los Angeles", "2017-11-07"},
	{"Donald Knuth", "docs", "Staff", "Milwaukee", "2022-01-10"},
	{"Ada Byron", "docs", "Senior", "London", "2023-08-21"},
	{"Katherine Johnson", "core", "Distinguished", "White Sulphur", "2016-08-26"},
	{"Margaret Hamilton", "tools", "Principal", "Paoli", "2019-08-17"},
	{"Radia Perlman", "core", "Distinguished", "Portsmouth", "2018-01-30"},
	{"Frances Allen", "tools", "Principal", "Peru", "2017-08-04"},
	{"Jean Bartik", "docs", "Senior", "Gentry", "2020-12-27"},
	{"Karen Spärck Jones", "core", "Distinguished", "Huddersfield", "2016-08-26"},
	{"Sophie Wilson", "tools", "Principal", "Leeds", "2021-06-14"},
	{"Anita Borg", "docs", "Staff", "Chicago", "2019-01-17"},
}

// searchDirectory is an ordinary function: what a query means is entirely
// the application's business.
func searchDirectory(_ context.Context, q string) ([]live.Choice, error) {
	var out []live.Choice
	for _, p := range directory {
		if q != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(q)) {
			continue
		}
		out = append(out, live.Choice{Value: p.Name, Label: p.Name + " · " + p.Team})
	}
	return out, nil
}

// Picker hosts a live.Combobox and shows what was chosen.
//
// Selecting is the child's action, so it re-renders the child. A parent
// that wants to show the result says so - here by pushing itself from
// OnSelect. That is scoped re-render working, not an oversight.
type Picker struct {
	shuttle.Base
	Chosen string
}

func newCombobox() shuttle.Component { return &Picker{} }

func (p *Picker) Render(ctx context.Context) templ.Component {
	box := shuttle.Child(ctx, "combobox", func() shuttle.Component {
		return &live.Combobox{
			Placeholder: "Find a person…",
			Empty:       "Nobody by that name.",
			Search:      searchDirectory,
			OnSelect: func(actx context.Context, c live.Choice) error {
				p.Chosen = c.Value
				return p.Push(actx)
			},
		}
	})

	return group(box,
		inside(text.New(text.Subtle, text.Small), label("Chosen: "+orNothing(p.Chosen))))
}

// loadDirectory is the table's data source: filter, sort, slice. A real one
// would be a query, and would push all three into the database.
func loadDirectory(_ context.Context, q live.Query) (live.Page[person], error) {
	needle := strings.ToLower(q.Filter)
	var matched []person
	for _, p := range directory {
		// Every column the table shows, so filtering something visible
		// always does something.
		haystack := strings.ToLower(strings.Join(
			[]string{p.Name, p.Team, p.Role, p.Place, p.Joined}, " "))
		if needle == "" || strings.Contains(haystack, needle) {
			matched = append(matched, p)
		}
	}

	sort.SliceStable(matched, func(i, j int) bool {
		less := sortKey(matched[i], q.Sort) < sortKey(matched[j], q.Sort)
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

// sortKey is the field a column sorts on. Unknown keys fall back to the
// name, so a stale sort in someone's bookmarked URL orders the table rather
// than emptying it.
func sortKey(p person, column string) string {
	switch column {
	case "team":
		return p.Team
	case "role":
		return p.Role
	case "place":
		return p.Place
	case "joined":
		return p.Joined
	default:
		return p.Name
	}
}

// teamTone colours the badge per team, so the column reads at a glance
// rather than being another word to parse.
func teamTone(team string) badge.Option {
	switch team {
	case "core":
		return badge.Blue
	case "tools":
		return badge.Emerald
	default:
		return badge.Amber
	}
}

// newTable builds a live.Table directly - it is a component in its own
// right, so it needs no wrapper.
func newTable() shuttle.Component {
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
				// Not sortable, and not for want of a key: a badge column is
				// a status, and sorting by colour is nobody's intent.
				Key: "team", Title: "Team", Width: "w-28",
				Cell: func(_ context.Context, p person) templ.Component {
					return inside(badge.New(teamTone(p.Team), badge.Small), label(p.Team))
				},
			},
			{
				Key: "place", Title: "Location", Sortable: true, Width: "w-40",
				Cell: func(_ context.Context, p person) templ.Component { return live.Text(p.Place) },
			},
			{
				Key: "joined", Title: "Joined", Sortable: true, Width: "w-32",
				Cell: func(_ context.Context, p person) templ.Component { return live.Text(p.Joined) },
			},
		},
		Load:       loadDirectory,
		PageSize:   5,
		Filterable: true,
		Choosable:  true,
		Empty:      "Nobody matches.",
	}
}
