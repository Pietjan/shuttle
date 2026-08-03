package live

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/dropdown"
	"github.com/pietjan/loom/icon"
	"github.com/pietjan/loom/input"
	"github.com/pietjan/loom/pagination"
	"github.com/pietjan/loom/table"
	loomtext "github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// Query is what a Table asks its data source for.
type Query struct {
	// Filter is the current search text.
	Filter string
	// Sort is the column key being sorted on, and Desc its direction.
	Sort string
	Desc bool
	// Offset and Limit are the page being shown.
	Offset, Limit int
}

// Page is what a data source answers with.
type Page[T any] struct {
	// Rows are the records for this page only. A Table never holds the
	// whole set, which is the point: the server keeps a page, not a table.
	Rows []T
	// Total is how many rows match the filter, across all pages, for the
	// pager to count with. Negative means unknown, and the pager falls back
	// to "there is more" rather than "page 3 of 9".
	Total int
}

// Column describes one column of a Table.
type Column[T any] struct {
	// Key identifies the column in a Query and in the URL. Keep it short
	// and stable: it is user-visible.
	Key string
	// Title is the header text.
	Title string
	// Cell renders one row's value for this column.
	Cell func(row T) templ.Component
	// Sortable allows the header to be clicked.
	Sortable bool
	// Width is a Tailwind width class for the column - "w-40", "w-1/4".
	// The table lays out fixed, so this is what the column gets; without
	// one it takes an equal share of what is left.
	Width string
}

// Table is a sorted, filtered, paginated view of a set the server owns.
//
//	&live.Table[User]{
//	    Columns: []live.Column[User]{
//	        {Key: "name", Title: "Name", Sortable: true,
//	         Cell: func(u User) templ.Component { return live.Text(u.Name) }},
//	    },
//	    Load: loadUsers,
//	}
//
// It never holds more than one page, so the size of the set behind it does
// not change what a connected tab costs.
//
// The view is in the URL - filter, sort and page - so it survives a reload,
// can be shared, and comes back after a reconnect. That is not decoration:
// a session lost to a restart is remounted from its URL and nothing else.
type Table[T any] struct {
	shuttle.Base

	// Columns in display order.
	Columns []Column[T]

	// Load fetches one page. It runs on the session's goroutine.
	Load func(ctx context.Context, q Query) (Page[T], error)

	// PageSize is how many rows to show. Zero means DefaultPageSize.
	PageSize int

	// Filterable shows the search field.
	Filterable bool

	// Choosable shows the column picker, a menu of every column with the
	// visible ones ticked.
	Choosable bool

	// Debounce is how long the filter waits after a keystroke.
	Debounce time.Duration

	// Empty is shown when nothing matches.
	Empty string

	page   Page[T]
	query  Query
	failed error
	hidden map[string]bool
}

// DefaultPageSize is a Table's page size when one is not set.
const DefaultPageSize = 25

func (t *Table[T]) Signals() map[string]any {
	return map[string]any{"filter": t.query.Filter}
}

// Query returns the view currently being shown.
func (t *Table[T]) Query() Query { return t.query }

// Visible returns the columns being shown, in display order.
//
// Hiding every column would leave a table that cannot be got back, so the
// last visible one refuses to hide - the picker greys it out and this
// enforces it, because the client's copy of a rule is a courtesy.
func (t *Table[T]) Visible() []Column[T] {
	out := make([]Column[T], 0, len(t.Columns))
	for _, col := range t.Columns {
		if !t.hidden[col.Key] {
			out = append(out, col)
		}
	}
	if len(out) == 0 && len(t.Columns) > 0 {
		return t.Columns[:1]
	}
	return out
}

// hiddenKeys is the hidden set in column order, for the URL.
func (t *Table[T]) hiddenKeys() []string {
	var out []string
	for _, col := range t.Columns {
		if t.hidden[col.Key] {
			out = append(out, col.Key)
		}
	}
	return out
}

// toggleColumn shows or hides one column.
func (t *Table[T]) toggleColumn(_ context.Context, key string) error {
	if t.hidden == nil {
		t.hidden = map[string]bool{}
	}
	if t.hidden[key] {
		delete(t.hidden, key)
		return nil
	}
	// Never the last one: a table with no columns has no picker to get them
	// back from.
	if len(t.Visible()) <= 1 {
		return nil
	}
	t.hidden[key] = true
	return nil
}

// Rows returns the current page's rows.
func (t *Table[T]) Rows() []T { return t.page.Rows }

// Err returns the last load failure, if any.
func (t *Table[T]) Err() error { return t.failed }

func (t *Table[T]) limit() int {
	if t.PageSize > 0 {
		return t.PageSize
	}
	return DefaultPageSize
}

func (t *Table[T]) debounce() time.Duration {
	if t.Debounce > 0 {
		return t.Debounce
	}
	return DefaultDebounce
}

// HandleParams reads the view out of the URL, so a shared link opens the
// same table - and so a session lost to a restart comes back where it was.
func (t *Table[T]) HandleParams(ctx context.Context, p shuttle.Params) error {
	t.query.Filter = p.Get("q")
	t.query.Sort = p.Get("sort")
	t.query.Desc = p.Get("dir") == "desc"

	t.hidden = map[string]bool{}
	for key := range strings.SplitSeq(p.Get("hide"), ",") {
		if key != "" {
			t.hidden[key] = true
		}
	}

	page, _ := strconv.Atoi(p.Get("page"))
	if page < 1 {
		page = 1
	}
	t.query.Limit = t.limit()
	t.query.Offset = (page - 1) * t.query.Limit

	return t.reload(ctx)
}

// QueryParams puts the view back into the URL after every render.
func (t *Table[T]) QueryParams() url.Values {
	q := url.Values{}
	if t.query.Filter != "" {
		q.Set("q", t.query.Filter)
	}
	if t.query.Sort != "" {
		q.Set("sort", t.query.Sort)
		if t.query.Desc {
			q.Set("dir", "desc")
		}
	}
	if n := t.pageNumber(); n > 1 {
		q.Set("page", strconv.Itoa(n))
	}
	// Hidden rather than visible, so the common case - everything showing -
	// leaves the URL alone.
	if hidden := t.hiddenKeys(); len(hidden) > 0 {
		q.Set("hide", strings.Join(hidden, ","))
	}
	return q
}

func (t *Table[T]) pageNumber() int {
	if t.query.Limit <= 0 {
		return 1
	}
	return t.query.Offset/t.query.Limit + 1
}

// reload asks the data source for the current view.
func (t *Table[T]) reload(ctx context.Context) error {
	t.failed = nil
	if t.Load == nil {
		t.page = Page[T]{}
		return nil
	}
	if t.query.Limit <= 0 {
		t.query.Limit = t.limit()
	}

	page, err := t.Load(ctx, t.query)
	if err != nil {
		// A failed load is the table's to report, not the page's to die of.
		t.page, t.failed = Page[T]{}, err
		return nil
	}
	t.page = page
	return nil
}

// filter re-reads the search field and returns to the first page, because
// staying on page 7 of a set that just shrank shows nothing.
func (t *Table[T]) filter(ctx context.Context) error {
	var f struct {
		Filter string `json:"filter"`
	}
	if err := shuttle.DecodeSignals(ctx, &f); err != nil {
		return err
	}
	if f.Filter == t.query.Filter {
		return nil
	}

	t.query.Filter = f.Filter
	t.query.Offset = 0
	return t.reload(ctx)
}

// sortBy toggles direction when the same column is clicked again.
// sortBy cycles one column: ascending, descending, then back to whatever
// order the source returns rows in.
//
// The third state is the one that gets left out, and it is the only way back:
// nothing else on a table un-sorts it, so without this a click on a heading
// is a decision you cannot undo - and the order the source chose, which is
// often the meaningful one, becomes unreachable for the rest of the session.
func (t *Table[T]) sortBy(ctx context.Context, key string) error {
	switch {
	case t.query.Sort != key:
		t.query.Sort, t.query.Desc = key, false
	case !t.query.Desc:
		t.query.Desc = true
	default:
		t.query.Sort, t.query.Desc = "", false
	}
	t.query.Offset = 0
	return t.reload(ctx)
}

// HasNext reports whether a further page exists.
func (t *Table[T]) HasNext() bool {
	if t.page.Total < 0 {
		return len(t.page.Rows) == t.query.Limit
	}
	return t.query.Offset+t.query.Limit < t.page.Total
}

// HasPrev reports whether this is not the first page.
func (t *Table[T]) HasPrev() bool { return t.query.Offset > 0 }

func (t *Table[T]) Render(ctx context.Context) templ.Component {
	visible := t.Visible()

	head := make([]templ.Component, 0, len(visible))
	for _, col := range visible {
		head = append(head, t.header(ctx, col))
	}

	body := make([]templ.Component, 0, t.limit())
	if len(t.page.Rows) == 0 {
		body = append(body, t.emptyRow(ctx, len(visible)))
	}
	for _, row := range t.page.Rows {
		cells := make([]templ.Component, 0, len(visible))
		for _, col := range visible {
			cells = append(cells, with(table.Cell(), t.cell(col, row)))
		}
		body = append(body, with(table.Row(), cells...))
	}

	// Blank rows to the page size. A last page with two rows on it would
	// otherwise be half the height of every other page, and the pager under
	// it would jump up the screen - most visibly while someone is typing in
	// the filter, which is when it moves most.
	//
	// They are spacers, not rows: no separator (the body draws those with
	// divide-y, so the padding turns it off), no hover, and aria-hidden, so
	// a screen reader is not told about five empty records that are really
	// just the shape of the page.
	for range t.limit() - len(body) {
		body = append(body, with(table.Row(
			table.Attr("aria-hidden", "true"),
			table.Class("border-0 hover:bg-transparent dark:hover:bg-transparent")),
			with(table.Cell(
				table.Attr("colspan", strconv.Itoa(max(len(visible), 1))),
				table.Class("h-[2.625rem] p-0")),
				templ.Raw("&nbsp;"))))
	}

	parts := []templ.Component{}
	if t.Filterable || t.Choosable {
		// The filter at one end, the column picker at the other: a table's
		// toolbar, and the same shape as the pager under it.
		parts = append(parts, seq(
			templ.Raw(`<div data-shuttle-table-tools class="mb-3 flex flex-wrap items-center justify-between gap-4">`),
			t.filterField(ctx),
			t.chooser(ctx),
			templ.Raw(`</div>`),
		))
	}

	// The table renders even with nothing in it: a filter that matches
	// nothing should not take the column headings away with it, and a region
	// that keeps its size is a region that does not throw the page around.
	parts = append(parts,
		with(table.New(shuttle.ID(ctx, table.ID, "table"),
			// Fixed, so the widths come from the headers rather than from
			// whichever rows this page happens to hold - otherwise every
			// sort and every page resizes every column.
			table.Class("table-fixed")),
			with(table.Header(), with(table.Row(), head...)),
			with(table.Body(), body...),
		),
	)

	return seq(append(parts, t.pager(ctx))...)
}

// header renders one column heading, clickable when the column sorts.
// header renders one column heading. A sortable one is a button, because
// activating it does something - but a ghost one at the heading's own size,
// so the row still reads as headings rather than as a toolbar someone left
// in the table.
//
// The sort direction is an icon rather than an arrow glyph: loom ships the
// set, and a caret that matches every other caret on the page beats a
// character that renders differently in every font.
func (t *Table[T]) header(ctx context.Context, col Column[T]) templ.Component {
	head := []table.Option{}
	if col.Width != "" {
		head = append(head, table.Class(col.Width))
	}
	if col.Sortable {
		head = append(head, table.Attr("aria-sort", t.sortState(col)))
	}

	if !col.Sortable {
		return with(table.Column(head...), text(col.Title))
	}

	key := col.Key
	sorter := button.New(
		button.Subtle, button.Tiny,
		button.Attr("data-shuttle-sort", key),
		// The heading's own type, not the button's: this is a th that can be
		// clicked, not a control that happens to sit in one.
		button.Class("-mx-2 gap-1 text-xs font-semibold uppercase tracking-wide -inset-x-[2px] text-base-500 dark:text-base-400"),
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			return t.sortBy(actx, key)
		}),
	)
	return with(table.Column(head...), with(sorter, text(col.Title), t.sortIcon(col)))
}

// sortIcon is the direction indicator, and on an unsorted column the
// affordance: a muted up-down caret, so a heading that can be sorted looks
// like one and the third state reads as "back to neutral" rather than as
// nothing having happened.
//
// Muted rather than absent, and one glyph rather than two, so a row of
// headings does not turn into a row of competing arrows.
func (t *Table[T]) sortIcon(col Column[T]) templ.Component {
	if t.query.Sort != col.Key {
		return icon.New(icon.CaretUpDown, icon.ExtraSmall,
			icon.Class("opacity-40"))
	}
	if t.query.Desc {
		return icon.New(icon.CaretDown, icon.ExtraSmall)
	}
	return icon.New(icon.CaretUp, icon.ExtraSmall)
}

// sortState is what a screen reader is told about this column, which is the
// same three states the icon shows.
func (t *Table[T]) sortState(col Column[T]) string {
	if !col.Sortable || t.query.Sort != col.Key {
		return "none"
	}
	if t.query.Desc {
		return "descending"
	}
	return "ascending"
}

func (t *Table[T]) cell(col Column[T], row T) templ.Component {
	if col.Cell == nil {
		return text("")
	}
	return col.Cell(row)
}

// emptyRow says why there is nothing, in a row spanning the table.
func (t *Table[T]) emptyRow(ctx context.Context, span int) templ.Component {
	msg := t.Empty
	if t.failed != nil {
		msg = "Could not load."
	} else if msg == "" {
		msg = "Nothing to show."
	}

	return with(table.Row(),
		with(table.Cell(
			table.Attr("colspan", strconv.Itoa(max(span, 1))),
			table.Attr("id", shuttle.ElementID(ctx, "empty")),
			table.Attr("data-shuttle-table-empty", ""),
			table.Class("text-center text-base-500 dark:text-base-400")),
			text(msg)))
}

// filterField is the search box, or nothing when the table has no filter.
func (t *Table[T]) filterField(ctx context.Context) templ.Component {
	if !t.Filterable {
		return empty
	}
	return input.New(
		shuttle.ID(ctx, input.ID, "filter"),
		shuttle.Bind(ctx, input.Attr, "filter"),
		shuttle.OnChange(ctx, input.Attr, t.debounce(), t.filter),
		input.Placeholder("Filter…"),
		// Small, so it lines up with the column picker's button beside it.
		input.Small,
		input.Class("max-w-64"),
		input.Attr("data-shuttle-table-filter", ""),
	)
}

// chooser is the column picker: loom's dropdown, one item per column, a
// check against the ones being shown.
//
// dropdown.Name is what makes it usable rather than a novelty. The menu is a
// popover, and a popover's open state lives on the element - so without a
// stable id the morph that follows every toggle would replace the panel and
// close it, and hiding two columns would mean opening the menu twice.
//
// The rows are buttons, not checkboxes: this changes the page immediately
// rather than collecting a choice to submit, and a button is what that is.
// The check is the state, and aria-pressed says so.
func (t *Table[T]) chooser(ctx context.Context) templ.Component {
	if !t.Choosable {
		return empty
	}

	items := make([]templ.Component, 0, len(t.Columns))
	for _, col := range t.Columns {
		key, shown := col.Key, !t.hidden[col.Key]
		last := shown && len(t.Visible()) <= 1

		options := []dropdown.Option{
			dropdown.Attr("data-shuttle-column", key),
			dropdown.Attr("aria-pressed", strconv.FormatBool(shown)),
		}
		if last {
			// The one column that cannot go, said in the markup as well as
			// enforced in the action.
			options = append(options, dropdown.Attr("aria-disabled", "true"))
		} else {
			options = append(options, shuttle.OnClick(ctx, dropdown.Attr,
				func(actx context.Context) error { return t.toggleColumn(actx, key) }))
		}

		items = append(items, with(dropdown.ItemButton(options...),
			t.checkMark(shown), text(col.Title)))
	}

	return with(dropdown.Root(dropdown.Name(shuttle.ElementID(ctx, "columns"))),
		with(dropdown.Trigger(), with(button.New(button.Outline, button.Small,
			button.Attr("data-shuttle-columns", "")),
			icon.New(icon.SlidersHorizontal, icon.ExtraSmall), text("Columns"))),
		with(dropdown.Menu(), items...),
	)
}

// checkMark keeps the menu's rows aligned whether or not they are ticked -
// an icon that appears and disappears would shift every label beside it.
func (t *Table[T]) checkMark(shown bool) templ.Component {
	if !shown {
		return templ.Raw(`<span class="size-4 shrink-0"></span>`)
	}
	return icon.New(icon.Check, icon.ExtraSmall)
}

// pager renders Loom's pagination: real links, because the page is in the
// URL and a page link that cannot be copied is a lie. __prevent is what
// keeps a click an action rather than a page load.
//
// Numbered pages need a total. A source that answers Total < 0 says it does
// not know, and gets Previous/Next around a page number instead - the
// honest version of "page 3 of ?".
func (t *Table[T]) pager(ctx context.Context) templ.Component {
	parts := []templ.Component{
		t.pageLink(ctx, pagination.Prev, t.pageNumber()-1, t.HasPrev(), "", "prev"),
	}

	if pages := t.pageCount(); pages > 1 {
		for _, n := range pageWindow(t.pageNumber(), pages) {
			if n == 0 {
				parts = append(parts, pagination.Gap())
				continue
			}
			parts = append(parts, t.pageLink(ctx, pagination.Item, n, true,
				strconv.Itoa(n), strconv.Itoa(n)))
		}
	}

	parts = append(parts,
		t.pageLink(ctx, pagination.Next, t.pageNumber()+1, t.HasNext(), "", "next"))

	// The count is not the pager: "1-4 of 6" is what the numbers cannot say,
	// and it is all there is to say when the source does not know the total.
	// They share a row, ends apart, which is where a table's footer puts
	// them.
	//
	// Tailwind classes, like loom's own components: an app compiling this
	// kit points @source at shuttle as well as at loom - see the README.
	// data-shuttle-pager-bar is there for one that wants to lay it out
	// differently.
	return seq(
		templ.Raw(fmt.Sprintf(`<div id=%q data-shuttle-pager-bar class=%q>`,
			shuttle.ElementID(ctx, "pager"),
			"mt-3 flex flex-wrap items-center justify-between gap-4")),
		with(loomtext.New(loomtext.Subtle, loomtext.Small,
			loomtext.Attr("data-shuttle-page-count", "")), text(t.count())),
		with(pagination.New(pagination.Attr("data-shuttle-pager", "")), parts...),
		templ.Raw(`</div>`),
	)
}

// pageLink builds one link.
func (t *Table[T]) pageLink(
	ctx context.Context,
	part func(href string, options ...pagination.Option) templ.Component,
	page int, enabled bool, label, hook string,
) templ.Component {
	// data-shuttle-page is shuttle's own hook, kept across the move to
	// Loom's markup: it is what the testing kit selects on and what an app
	// styling a pager already reaches for.
	marker := pagination.Attr("data-shuttle-page", hook)
	if !enabled {
		// No href, which is how pagination renders a control inert - and no
		// action, so a click that somehow arrives does nothing.
		return part("", marker)
	}

	options := []pagination.Option{
		marker,
		shuttle.OnEvent(ctx, pagination.Attr, "click__prevent", func(actx context.Context) error {
			return t.goTo(actx, page)
		}),
	}
	if page == t.pageNumber() {
		options = append(options, pagination.Current())
	}
	if label == "" {
		return part(t.href(page), options...)
	}
	return with(part(t.href(page), options...), text(label))
}

// href is this table's view with one page number changed, so the link is
// the address someone would land on by following it.
func (t *Table[T]) href(page int) string {
	q := t.QueryParams()
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	} else {
		q.Del("page")
	}
	if len(q) == 0 {
		return "?"
	}
	return "?" + q.Encode()
}

// goTo jumps to a page, clamped: the markup a click was sent against may
// already be a page or two stale.
func (t *Table[T]) goTo(ctx context.Context, page int) error {
	if page < 1 {
		page = 1
	}
	if pages := t.pageCount(); pages > 0 && page > pages {
		page = pages
	}
	t.query.Offset = (page - 1) * t.query.Limit
	return t.reload(ctx)
}

// pageCount is how many pages there are, or 0 when the source does not say.
func (t *Table[T]) pageCount() int {
	if t.page.Total < 0 || t.query.Limit <= 0 {
		return 0
	}
	return (t.page.Total + t.query.Limit - 1) / t.query.Limit
}

// pageWindow is the list of page numbers to render, with 0 standing for a
// gap: always the first and last, and the current page with a neighbour
// either side. Nine pages fit; nine hundred would not.
func pageWindow(current, pages int) []int {
	const span = 1

	want := map[int]bool{1: true, pages: true}
	for n := current - span; n <= current+span; n++ {
		if n >= 1 && n <= pages {
			want[n] = true
		}
	}

	out := make([]int, 0, len(want)+2)
	for n := 1; n <= pages; n++ {
		if want[n] {
			out = append(out, n)
			continue
		}
		// One gap per run, not one per page it stands for.
		if len(out) > 0 && out[len(out)-1] != 0 {
			out = append(out, 0)
		}
	}
	return out
}

// count describes the page, and says less when Total is unknown rather than
// inventing a number.
func (t *Table[T]) count() string {
	if t.page.Total < 0 {
		return "Page " + strconv.Itoa(t.pageNumber())
	}
	if t.page.Total == 0 {
		return "No rows"
	}

	from := t.query.Offset + 1
	to := t.query.Offset + len(t.page.Rows)
	return strings.Join([]string{
		strconv.Itoa(from), "–", strconv.Itoa(to), "of", strconv.Itoa(t.page.Total),
	}, " ")
}

// Text renders a string as a table cell's contents, for the common case
// where a column is just a field.
func Text(s string) templ.Component { return text(s) }
