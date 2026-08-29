package live_test

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

type user struct{ Name, Team string }

// everyone is the set behind the table. It stays on the server; only a page
// of it is ever rendered.
var everyone = []user{
	{"Ada", "core"},
	{"Alan", "core"},
	{"Grace", "tools"},
	{"Edsger", "tools"},
	{"Barbara", "core"},
	{"Donald", "docs"},
}

// loadUsers is an ordinary data source: filter, sort, slice.
func loadUsers(_ context.Context, q live.Query) (live.Page[user], error) {
	var matched []user
	for _, u := range everyone {
		if q.Filter == "" ||
			strings.Contains(strings.ToLower(u.Name), strings.ToLower(q.Filter)) ||
			strings.Contains(strings.ToLower(u.Team), strings.ToLower(q.Filter)) {
			matched = append(matched, u)
		}
	}

	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		less := a.Name < b.Name
		if q.Sort == "team" {
			less = a.Team < b.Team
		}
		if q.Desc {
			return !less
		}
		return less
	})

	total := len(matched)
	if q.Offset >= total {
		return live.Page[user]{Total: total}, nil
	}
	end := min(q.Offset+q.Limit, total)
	return live.Page[user]{Rows: matched[q.Offset:end], Total: total}, nil
}

func newTable() *live.Table[user] {
	return &live.Table[user]{
		Columns: []live.Column[user]{
			{
				Key: "name", Title: "Name", Sortable: true,
				Cell: func(_ context.Context, u user) templ.Component { return live.Text(u.Name) },
			},
			{
				Key: "team", Title: "Team", Sortable: true,
				Cell: func(_ context.Context, u user) templ.Component { return live.Text(u.Team) },
			},
		},
		Load:       loadUsers,
		PageSize:   2,
		Filterable: true,
		Choosable:  true,
	}
}

// rowNames reads the first column out of the rendered table.
func rowNames(t *testing.T, l *shuttle.Live) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(l.HTML(), `data-ui="table-cell"`)[1:] {
		start := strings.IndexByte(line, '>')
		end := strings.Index(line, "</td>")
		if start < 0 || end < start {
			continue
		}
		out = append(out, strings.TrimSpace(line[start+1:end]))
	}
	// Two columns per row; the first of each pair is the name.
	var names []string
	for i := 0; i < len(out); i += 2 {
		names = append(names, out[i])
	}
	return names
}

// TestTableShowsOnePage. The set behind it can be any size: what a
// connected tab costs does not depend on it.
func TestTableShowsOnePage(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	if got := rowNames(t, l); len(got) != 2 {
		t.Fatalf("rows = %v, want 2 of them", got)
	}
	if len(tbl.Rows()) != 2 {
		t.Errorf("the component is holding %d rows, want one page", len(tbl.Rows()))
	}
	l.Assert().
		TextContains("[data-shuttle-page-count]", "of 6").
		NoDuplicateIDs()
}

// TestTableSortsAndTogglesDirection.
func TestTableSortsAndTogglesDirection(t *testing.T) {
	l := shuttle.Test(t, newTable())

	// Default order is by name.
	if got := rowNames(t, l); got[0] != "Ada" {
		t.Errorf("first row = %q, want Ada", got[0])
	}

	// Clicking the sorted column again reverses it.
	l.Click(`[data-shuttle-sort="name"]`)
	l.Click(`[data-shuttle-sort="name"]`)
	if got := rowNames(t, l); got[0] != "Grace" {
		t.Errorf("descending first row = %q, want Grace", got[0])
	}
	l.Assert().URL("/?dir=desc&sort=name")

	// A different column starts ascending again.
	l.Click(`[data-shuttle-sort="team"]`)
	if q := l.Component().(*live.Table[user]).Query(); q.Sort != "team" || q.Desc {
		t.Errorf("query = %+v, want an ascending sort on team", q)
	}
}

// TestTableFiltersAndReturnsToTheFirstPage. Staying on page 3 of a set that
// just shrank shows nothing at all.
func TestTableFiltersAndReturnsToTheFirstPage(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	l.Click(`[data-shuttle-page="next"]`)
	if !tbl.HasPrev() {
		t.Fatal("did not move off the first page")
	}

	l.Signal("filter", "tools").Change("[data-shuttle-table-filter]")

	if tbl.HasPrev() {
		t.Error("filtering left the table on a later page")
	}
	if got := rowNames(t, l); len(got) != 2 {
		t.Errorf("rows = %v, want the two tools members", got)
	}
	l.Assert().URL("/?q=tools")
}

// TestTablePagerStopsAtTheEnds. A disabled control is an inert
// <span aria-disabled>, not an <a disabled> - the disabled attribute does
// nothing on a link, which is why pagination drops the href instead.
func TestTablePagerStopsAtTheEnds(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	l.Assert().Exists(`[data-shuttle-page="prev"][aria-disabled]`)

	l.Click(`[data-shuttle-page="next"]`)
	l.Click(`[data-shuttle-page="next"]`)
	if tbl.HasNext() {
		t.Error("HasNext on the last page")
	}
	l.Assert().Exists(`[data-shuttle-page="next"][aria-disabled]`)

	// Going back returns to the start and re-disables previous.
	l.Click(`[data-shuttle-page="prev"]`)
	l.Click(`[data-shuttle-page="prev"]`)
	if tbl.HasPrev() {
		t.Error("HasPrev on the first page")
	}
}

// TestTableViewIsInTheURL, so it can be shared - and so a session lost to a
// restart is remounted where it was.
func TestTableViewIsInTheURL(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	l.Params("q=core&sort=team&dir=desc&page=2")

	q := tbl.Query()
	if q.Filter != "core" || q.Sort != "team" || !q.Desc {
		t.Errorf("query = %+v, want the view from the URL", q)
	}
	if q.Offset != 2 {
		t.Errorf("offset = %d, want the second page", q.Offset)
	}
}

// TestTableSurvivesAFailingLoad.
func TestTableSurvivesAFailingLoad(t *testing.T) {
	boom := errors.New("database is away")
	tbl := newTable()
	tbl.Load = func(context.Context, live.Query) (live.Page[user], error) {
		return live.Page[user]{}, boom
	}
	l := shuttle.Test(t, tbl)

	if !errors.Is(tbl.Err(), boom) {
		t.Errorf("Err() = %v, want the load failure", tbl.Err())
	}
	l.Assert().TextContains("[data-shuttle-table-empty]", "Could not load")
}

// TestTableWithAnUnknownTotal says less rather than inventing a number.
func TestTableWithAnUnknownTotal(t *testing.T) {
	tbl := newTable()
	tbl.Load = func(_ context.Context, q live.Query) (live.Page[user], error) {
		page, _ := loadUsers(context.Background(), q)
		page.Total = -1
		return page, nil
	}
	l := shuttle.Test(t, tbl)

	l.Assert().Text("[data-shuttle-page-count]", "Page 1")
	if !tbl.HasNext() {
		t.Error("a full page with an unknown total should offer a next page")
	}
}

// TestTableEmpty.
func TestTableEmpty(t *testing.T) {
	tbl := newTable()
	tbl.Empty = "No one here."
	l := shuttle.Test(t, tbl)

	l.Signal("filter", "nobody").Change("[data-shuttle-table-filter]")
	l.Assert().
		Text("[data-shuttle-table-empty]", "No one here.").
		Text("[data-shuttle-page-count]", "No rows")
}

// TestTableWithoutALoadRenders rather than panicking, so a half-configured
// table fails visibly instead of taking the page down.
func TestTableWithoutALoadRenders(t *testing.T) {
	l := shuttle.Test(t, &live.Table[user]{
		Columns: []live.Column[user]{{Key: "name", Title: "Name"}},
	})
	l.Assert().Exists("[data-shuttle-table-empty]")
}

// TestTableColumnPickerHidesAndRestores. The picker changes what the table
// shows, so its state belongs in the URL with the rest of the view.
func TestTableColumnPickerHidesAndRestores(t *testing.T) {
	l := shuttle.Test(t, newTable())

	l.Assert().Exists(`[data-shuttle-column="team"][aria-pressed="true"]`)

	l.Click(`[data-shuttle-column="team"]`)
	l.Assert().
		Missing(`[data-shuttle-sort="team"]`).
		Exists(`[data-shuttle-column="team"][aria-pressed="false"]`).
		URL("/?hide=team")

	l.Click(`[data-shuttle-column="team"]`)
	l.Assert().Exists(`[data-shuttle-sort="team"]`).URL("/")
}

// TestTableKeepsOneColumn: hiding the last one would leave a table with no
// picker to get the others back from. The markup says so with aria-disabled
// and the action enforces it, because a client's copy of a rule is a
// courtesy.
func TestTableKeepsOneColumn(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	l.Click(`[data-shuttle-column="team"]`)
	if got := len(tbl.Visible()); got != 1 {
		t.Fatalf("visible columns = %d, want 1", got)
	}

	// Marked, and given no action at all - the click that would hide it is
	// not rendered rather than rendered and refused.
	l.Assert().Exists(`[data-shuttle-column="name"][aria-disabled="true"]`)
	if strings.Contains(l.HTML(), `data-shuttle-column="name" aria-pressed="true" data-on:click`) {
		t.Error("the last visible column still has a click binding")
	}

	// And a URL that asks for the impossible is refused too, because a
	// client's copy of a rule is a courtesy.
	bare := newTable()
	shuttle.Test(t, bare).Params("?hide=name,team")
	if got := len(bare.Visible()); got != 1 {
		t.Errorf("a URL hiding every column left %d visible, want 1", got)
	}
}

// TestTableHiddenColumnsComeBackFromTheURL, which is what a shared link and
// a reconnect both depend on.
func TestTableHiddenColumnsComeBackFromTheURL(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl).Params("?hide=team")

	l.Assert().Missing(`[data-shuttle-sort="team"]`)
	if got := len(tbl.Visible()); got != 1 {
		t.Errorf("visible columns = %d, want the one the URL left", got)
	}
}

// TestTableKeepsItsShape. A table that resizes on every sort, page and
// keystroke throws the page around under the cursor - so a short page is
// padded to the page size and the headings stay put when nothing matches.
func TestTableKeepsItsShape(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	// Rows carry no marker of their own, so count the body's.
	rows := func() int {
		html := l.HTML()
		body := html[strings.Index(html, `data-ui="table-body"`):]
		return strings.Count(body[:strings.Index(body, "</tbody>")], "<tr")
	}

	full := rows()
	if full != tbl.PageSize {
		t.Fatalf("a full page rendered %d rows, want %d", full, tbl.PageSize)
	}

	// A filter that matches one row still renders a page's worth.
	l.Signal("filter", "ada").Change(`[data-shuttle-table-filter]`)
	if got := rows(); got != full {
		t.Errorf("a short page rendered %d rows, want the page's %d", got, full)
	}

	// And one that matches nothing keeps the table, headings included.
	l.Signal("filter", "nobody at all").Change(`[data-shuttle-table-filter]`)
	l.Assert().
		Exists(`[data-shuttle-table-empty]`).
		Exists(`[data-shuttle-sort="name"]`)
	if got := rows(); got != full {
		t.Errorf("an empty table rendered %d rows, want the page's %d", got, full)
	}
}

// TestTableLaysOutFixed: with content-sized columns, every page is a
// different set of widths.
func TestTableLaysOutFixed(t *testing.T) {
	l := shuttle.Test(t, newTable())
	if !strings.Contains(l.HTML(), "table-fixed") {
		t.Error("the table is not fixed-layout, so its columns resize per page")
	}
}

// TestTableSortCyclesBackToUnsorted. Ascending, descending, then the order
// the source returns - the third state is the only way back, since nothing
// else on a table un-sorts it.
func TestTableSortCyclesBackToUnsorted(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl)

	l.Assert().Exists(`th[aria-sort="none"]`)

	l.Click(`[data-shuttle-sort="name"]`)
	l.Assert().Exists(`th[aria-sort="ascending"]`).URL("/?sort=name")

	l.Click(`[data-shuttle-sort="name"]`)
	l.Assert().Exists(`th[aria-sort="descending"]`).URL("/?dir=desc&sort=name")

	l.Click(`[data-shuttle-sort="name"]`)
	l.Assert().Exists(`th[aria-sort="none"]`).URL("/")
	if tbl.Query().Sort != "" || tbl.Query().Desc {
		t.Errorf("third click left sort=%q desc=%v, want neither", tbl.Query().Sort, tbl.Query().Desc)
	}

	// A different column starts its own cycle rather than inheriting the
	// direction of the one before it.
	l.Click(`[data-shuttle-sort="name"]`)
	l.Click(`[data-shuttle-sort="name"]`)
	l.Click(`[data-shuttle-sort="team"]`)
	if q := tbl.Query(); q.Sort != "team" || q.Desc {
		t.Errorf("switching column gave sort=%q desc=%v, want team ascending", q.Sort, q.Desc)
	}
}

// TestAPagePastTheEndSnapsBack. ?page=99 arrives in shared links and in
// URLs whose set shrank since they were copied; without the snap the pager
// renders "491-490 of 15" around an empty body.
func TestAPagePastTheEndSnapsBack(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl).Params("page=99")

	if got := tbl.Query().Offset; got != 4 {
		t.Errorf("offset = %d, want the last page's 4", got)
	}
	if names := rowNames(t, l); len(names) == 0 {
		t.Error("the snapped-to page rendered no rows")
	}
	if strings.Contains(l.HTML(), "of 15</") && strings.Contains(l.HTML(), "197") {
		t.Errorf("count line still describes the impossible page")
	}
}

// TestHidingTheSortedColumnUnsorts. The third click on a heading is the
// only way back to the source's own order, and hiding that column takes
// the control off the screen - so hiding it is the un-sort, rather than
// data staying ordered by a key nobody can see or reach.
func TestHidingTheSortedColumnUnsorts(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl).Params("sort=team")

	if tbl.Query().Sort != "team" {
		t.Fatal("the sort did not arrive from the URL")
	}
	l.Click(`[data-shuttle-column="team"]`)

	if got := tbl.Query().Sort; got != "" {
		t.Errorf("sort = %q after hiding its column, want unsorted", got)
	}
	if _, ok := tbl.QueryParams()["sort"]; ok {
		t.Error("the URL still carries a sort nobody can cycle off")
	}
}

// TestHidingEverythingRepairsTheState. Visible falls back to the first
// column, and the rest of the component has to follow: a picker rendering
// that column unticked, and a URL still hiding it, would both describe a
// table that is not on the screen.
func TestHidingEverythingRepairsTheState(t *testing.T) {
	tbl := newTable()
	l := shuttle.Test(t, tbl).Params("hide=name,team")

	if got := len(tbl.Visible()); got != 1 {
		t.Fatalf("%d visible columns, want the fallback of 1", got)
	}
	shown := tbl.Visible()[0].Key
	if hidden := tbl.QueryParams().Get("hide"); strings.Contains(hidden, shown) {
		t.Errorf("hide=%q still names the column on screen", hidden)
	}
	if !strings.Contains(l.HTML(), `data-shuttle-column="`+shown+`" aria-pressed="true"`) {
		t.Error("the picker does not tick the column that is showing")
	}
}

// TestTwoTablesShareAURLThroughParam. The URL keys are literals, so two
// tables on one page would fight over q, sort and page - Param is the
// namespace that keeps them apart, and both views survive a round trip
// through one query string.
func TestTwoTablesShareAURLThroughParam(t *testing.T) {
	a, b := newTable(), newTable()
	a.Param, b.Param = "a", "b"

	if err := a.HandleParams(context.Background(), paramsOf("a-q=core&b-q=infra")); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleParams(context.Background(), paramsOf("a-q=core&b-q=infra")); err != nil {
		t.Fatal(err)
	}

	if got := a.Query().Filter; got != "core" {
		t.Errorf("table a filter = %q, want its own key's value", got)
	}
	if got := b.Query().Filter; got != "infra" {
		t.Errorf("table b filter = %q, want its own key's value", got)
	}
	if got := a.QueryParams().Get("a-q"); got != "core" {
		t.Errorf("table a writes a-q=%q, want core", got)
	}
}

func paramsOf(qs string) shuttle.Params {
	v, _ := url.ParseQuery(qs)
	return shuttle.Params(v)
}
