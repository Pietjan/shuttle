# shuttle

Stateful, server-driven UI for Go. A component is a struct, its exported methods are the actions,
and `Render` returns a [templ](https://templ.guide) component. State lives in server memory for as
long as the page is connected, so a click runs a Go closure over that state and the page updates
itself.

It renders with [loom](https://github.com/pietjan/loom) and talks to the browser with
[datastar](https://data-star.dev). A shuttle carries the weft thread back and forth across a loom,
which is the job: state, across the wire, both directions.

**Status: early.** The core works and is tested, but the API is not stable and there is no release
yet. See [What works](#what-works) for the honest boundary.

## A counter

```go
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

func main() {
    http.ListenAndServe(":8080", shuttle.New(func() shuttle.Component {
        return &Counter{}
    }))
}
```

The first `GET` returns a complete server-rendered document that works before any script runs. The
page then opens one persistent SSE stream and attaches to the markup already in the DOM. A click
posts to an opaque action URL, the closure runs, and the component's new markup comes back down
the stream and is morphed in.

Because state never leaves the server, actions are **closures registered per render** rather than
named strings looked up by reflection. They are type-checked, they capture loop variables, and the
client can only invoke actions that were rendered for it.

That is the whole of `example/counter`:

```bash
go run ./example/counter
```

Everything else — forms, nesting, pub/sub and presence, streams, uploads, navigation, and the live
component kit — has a focused example with its source alongside it:

```bash
make run
```

The site is built out of Loom — its navigation, headings and source blocks are the same components
the examples use — so it needs Loom's classes compiled, which `make run` does first. That is the
same two commands any app using Loom runs, and worth copying rather than working around:

```bash
go run github.com/pietjan/loom/cmd/css -o example/site/css/loom.css
tailwindcss -i example/site/css/input.css -o example/site/static/styles.css --minify
```

`cmd/css` writes Tailwind, Loom's theme and its structural CSS, with an `@source` pointing at
Loom's module directory — so the sheet carries every class baked into Loom's components. The entry
file next to it imports that and adds one more source:

```css
@import "./loom.css";
@source "../../..";   /* this repository */
```

**That second source is not optional if you use `live/`.** The kit's markup — the table's pager,
its headings — is Tailwind classes, exactly as Loom's components are, so a build that scans only
Loom compiles a page whose pager has no layout. Point `@source` at shuttle's module directory
alongside Loom's:

```css
@source "../../go/pkg/mod/github.com/pietjan/shuttle@v0.1.0";
```

Compile the sheet for **your** app rather than borrowing another project's build: a stylesheet also
carries that project's choices, and Loom's own site redefines the `dark:` variant as class-based
for a theme toggle it has and you may not.

## Bring your own CSS

Loom's markup is Tailwind, and compiling it is the app's job — shuttle ships no stylesheet. Point
`Head` at yours:

```go
h := shuttle.New(func() shuttle.Component { return &Counter{} })
h.Title = "my app"
h.Head = `<link rel="stylesheet" href="/static/app.css">`
```

For full control of the document — a theme class on `<html>`, your own meta tags and scripts — set
`Shell`. `Page.Attach` must end up in a `data-init` attribute **on `<body>`**: anything inside the
component's markup is re-initialised by every patch and would open a second stream.

## Mounting in a real app

Set `Prefix` to wherever you route to it, and shuttle builds its own URLs to match:

```go
h := shuttle.New(func() shuttle.Component { return &Dashboard{} })
h.Prefix = "/dashboard"

mux := http.NewServeMux()
mux.Handle("/dashboard/", h)
mux.HandleFunc("GET /{$}", home)
```

The page is served at exactly the mount point, not as a catch-all — a catch-all would mint a
session for every `/favicon.ico` a crawler asks for, and each one costs a component tree.

## Client-side state

Some state belongs on the client: what is being typed, whether a disclosure is open. Declare it and
shuttle namespaces it per component instance, so two components declaring `query` don't silently
share one value, and scopes every action's payload to that namespace — otherwise Datastar sends the
entire global signal store on every event.

```go
func (s *Search) Signals() map[string]any {
    return map[string]any{"query": ""}
}

func (s *Search) Render(ctx context.Context) templ.Component {
    box := input.New(
        shuttle.ID(ctx, input.ID, "query"),
        shuttle.Bind(ctx, input.Attr, "query"),
    )
    // ...
}
```

Read it in an action with `shuttle.DecodeSignals(ctx, &dest)`. Anything the struct doesn't declare
is ignored, so the client can only reach fields you asked for.

### While an action is running

A slow action can say so without a round trip to admit it. `shuttle.Indicator` marks the element
with a signal that is true for as long as the request it started is in flight:

```go
save := button.New(button.Primary,
    shuttle.OnClick(ctx, button.Attr, s.save, shuttle.Indicator("saving")),
    button.Attr("data-attr:disabled", shuttle.IndicatorRef(ctx, "saving")),
)
```

The action POST doesn't return until your closure has finished, so the signal covers exactly the
work. It lives under `ind.`, outside the component's own namespace, which is what keeps it out of
every action payload and out of `DecodeSignals` — the server never reads it, so it never travels.
The path names the component instance, so two of the same component don't share a spinner — which
is why the markup reads it with `IndicatorRef` rather than spelling it out: a hard-coded
`$ind.c.saving` works while the component is the root and quietly watches the wrong signal the
moment it is mounted as somebody's child.

## Forms

`OnChange` validates as you type; `OnSubmit` commits. `OnChange` listens on `input` rather than
`change` — `change` waits for blur, too late to be useful — so it fires per keystroke and its
debounce argument is not optional.

`Validation` pipes straight into Loom's `field.Error`, which is a no-op on the empty string
precisely so results can be passed through unconditionally:

```go
field.Root(field.Error(f.Errors.For("email")))
```

## Nesting

`shuttle.Child(ctx, key, factory)` mounts a child with its own state, action table and morph
target, so an event on it re-renders it alone.

**The key is the whole identity rule**: the same key keeps the same instance and its state across
the parent's re-renders, a different key mounts fresh, and a key you stop rendering is unmounted.
The factory runs only on that first mount, so props captured in it are mount-time props — to update
a live child, hold a reference and set its fields.

`Base.Emit` sends an event up to the nearest ancestor implementing `Receiver`.

## Navigation

`HandleParams` runs whenever the URL changes without a reload — a filter applied, the back button
pressed — and on the first render too, so reading the URL needs only that hook.

```go
func (t *Table) HandleParams(_ context.Context, p shuttle.Params) error {
    t.Filter = p.Get("filter")
    return nil
}

// QueryParams syncs state into the address bar, so the view can be shared.
func (t *Table) QueryParams() url.Values {
    return url.Values{"filter": {t.Filter}}
}
```

`Navigate` pushes a history entry, `Replace` doesn't, `Redirect` is a full page load that throws
the session away. `QueryParams` syncs with `replaceState`, so a filter box doesn't fill the back
button with one entry per keystroke.

Back and forward are the only part the server can't see, so shuttle ships one small `popstate`
shim. It reaches the page through `Page.Scripts` — a custom `Shell` that drops it loses those
buttons.

## Real time

The stream is already open, which is the reason to hold state on the server at all.

```go
func (r *Room) Mount(ctx context.Context, _ shuttle.Params) error {
    return r.Join(ctx, "lobby", currentUser)      // presence + subscription
}

func (r *Room) HandleInfo(ctx context.Context, msg any) error {
    // pub/sub messages, presence events, timer ticks
}
```

`Subscribe`/`Publish` for pub/sub, `Every` for timers, `Join`/`Presence` for who is connected. The
`Broker` interface has an in-memory implementation; swap in Redis or NATS for more than one node
and nothing above it changes.

A roster goes to everyone on the topic, so `Member.Tag` names a page without being the thing that
unlocks it — attach whatever your app needs to identify someone as the `meta` argument to `Join`,
and read it back as `Member.Meta`.

For long collections, `Base.Stream` patches a container an item at a time, so the component never
holds the collection:

```go
s := c.Stream("log")
s.Append(ctx, key, entry)   // also Prepend, Replace, Remove, Clear
```

## File uploads

Uploads get their own request: the stream only goes one way, so bytes cannot travel up it.

```go
func (g *Gallery) Uploads() []shuttle.Upload {
    return []shuttle.Upload{{
        Name:     "photos",
        MaxSize:  5 << 20,
        MaxFiles: 4,
        Accept:   []string{"image/*"},
    }}
}

func (g *Gallery) HandleUpload(ctx context.Context, name string, files []*shuttle.UploadedFile) error {
    for _, f := range files {
        if _, err := f.Save("/var/app/uploads"); err != nil {
            return err
        }
    }
    return nil
}
```

Render the picker with `shuttle.FileInput(ctx, input.Attr, "photos")`.

Files stream to temp files rather than into memory — a component already costs memory per connected
tab, and buffering whole uploads on top of that turns one large file into an outage. **They are
deleted when `HandleUpload` returns**, so copy out what you want to keep. `Save` cleans the client's
filename first: `../../etc/passwd` is exactly what an upload endpoint gets sent.

Every limit is enforced again on the server. The `accept` attribute and any client-side check are
courtesies to the user; the client's copy of the rules is trivially skipped.

`Accept` is checked against **the file's own first bytes**, not the content type the client
declared — that one is a string an attacker writes, so checking it would let an executable through
by calling itself an image. `UploadedFile.Type` is what was detected and `DeclaredType` is what was
claimed. Detection is signature-based, so a container format arrives as its container: a `.docx` is
a zip. Text is handled (any `text/*` entry accepts detected text, since everything textual detects
as `text/plain`); for the rest there is `Upload.TrustDeclaredType`, which does what it says and
makes `Accept` a courtesy for that upload.

Progress uses `XMLHttpRequest`, because `fetch` reports none — that's the whole reason uploads need
their own path. During an upload the input carries `data-shuttle-uploading` and
`data-shuttle-progress`, and a `--shuttle-progress` custom property, so a progress bar costs no
round trips:

```css
input[data-shuttle-uploading] + .bar { width: var(--shuttle-progress); }
```

## The live component kit

`shuttle/live` holds the components Loom lists under "Known limitations" as left out rather than
approximated, because they cannot be built without somewhere for the query to go.

**Combobox** — filter-as-you-type over a set the server owns:

```go
shuttle.Child(ctx, "user", func() shuttle.Component {
    return &live.Combobox{Placeholder: "Find a user…", Search: findUsers, OnSelect: pick}
})
```

The query lives on the client, so typing costs nothing until the debounce expires, and the set never
reaches the browser. Rows are buttons and the arrow keys move **real focus** between them — which
keeps Enter native, and follows Loom's own reasoning that `role="option"` on a row announces one
thing while activating it does another.

**Table** — sorted, filtered, paginated, holding one page and never the whole set:

```go
&live.Table[User]{
    Columns: []live.Column[User]{{Key: "name", Title: "Name", Sortable: true, Cell: ...}},
    Load:    loadUsers,
    Filterable: true,
}
```

The view lives in the URL, so it can be shared, survives a reload, and comes back after a restart —
which is the same mechanism reconnect recovery relies on.

## Testing your components

`shuttle.Test` drives mount → action → render with no browser and no HTTP server:

```go
func TestTodo(t *testing.T) {
    live := shuttle.Test(t, &Todo{})

    live.Signal("draft", "buy milk").Click("#shuttle-c-add")

    live.Assert().
        Count("li.item", 1).
        Text("li.item", "buy milk").
        NoDuplicateIDs()
}
```

`Click`, `Submit`, `Change` fire bindings; `Signal` sets client state the action will carry;
`Params` changes the URL the way a filter or the back button would; `Publish` delivers a pub/sub
message so `HandleInfo` can be tested without a second page. `Patches()` returns what was pushed,
for streams and server pushes.

Assertions take a small selector — tags, `#id`, `.class`, `[attr=value]`, and descendants — and
each reports rather than stopping, so one failure doesn't hide the next. **`NoDuplicateIDs` is
worth calling in every component's test**, since that failure is otherwise completely silent.

What the kit asserts on is the markup the session actually pushed, not a render performed for the
test — so a component whose action renders nothing fails here exactly as it would in a page.

`live.Component()` returns your struct, which is often the clearest thing to assert on.

## One session for a whole section

Every full page load is a new session and a new stream, so a site whose links reload the page burns
a connection per page. `Handler.Subtree` lets one handler own a path subtree instead, and one
session render all of it:

```go
h := shuttle.New(func() shuttle.Component { return &Site{} })
h.Prefix = "/app"
h.Subtree = true          // every path under /app is this component's page
mux.Handle("/app/", h)
```

The component reads `Base.Path()` to decide what to render, and links become actions rather than
`href`s:

```go
shuttle.OnClick(ctx, button.Attr, func(ctx context.Context) error {
    return s.Navigate(ctx, "/app/reports/")
})
```

`Navigate` pushes the path into history from the server and re-renders, so the address bar, the
back button and deep links all keep working — the page just never reloads. The examples site is
built this way: browsing all nine examples three times over uses **one** stream, where the same
browsing with `href` links used to run the browser out of connections after four.

Only mount a subtree handler where the app routed to it deliberately. At the server root it would
mint a session for every `/favicon.ico` a crawler asks for.

## Serve it over HTTP/2

This is a deployment requirement, not a preference.

Datastar streams over `fetch`, not `EventSource`, so **every connected page holds one of the
browser's ~6 connections per origin** for as long as it is open — and that budget is shared across
tabs. Five or six live pages and the next one simply never loads: no error, no console message, the
request just never gets a connection to go out on.

Over HTTP/2 every stream shares a single connection and the limit stops existing. Measured, one
client with six concurrent streams:

| transport | TCP connections |
|---|---|
| HTTP/1.1 | 6, and the browser will not open a seventh |
| HTTP/2 | 1 |

Browsers only speak HTTP/2 over TLS, so in practice: terminate TLS at your proxy with HTTP/2
enabled, or serve TLS directly. For local development `go run ./example/site -tls` demonstrates it
with a throwaway certificate.

There is no good workaround on HTTP/1.1. `Handler.OpenWhenHidden` defaults to **true**, following
[Datastar's own guidance](https://data-star.dev/how_tos/prevent_sse_connections_closing), and for a
framework holding state per page it is close to required: turn it off and a hidden tab's stream
closes, `Handler.Grace` later evicts the session, and returning to that tab reloads the page and
loses what it held. Trading that away to reclaim a connection is only reasonable on HTTP/1.1 with
`Grace` raised to cover how long a tab might sit in the background.

## Connection state

The page puts its connection state on `<html>`, so you can style it:

```css
.shuttle-reconnecting .banner { display: block; }
```

`shuttle-connecting`, `shuttle-connected`, `shuttle-reconnecting`, `shuttle-dead` — also available
as `data-shuttle-state`. When Datastar gives up for good, the page polls a session-free health
endpoint and reloads once the server answers.

**A server restart recovers on its own.** Every open page holds a session the new process has never
heard of, so the stream endpoint answers those with a reload rather than a 404 that would be
retried forever. The page comes back with whatever state its URL carries — which is the argument
for putting shareable state in `QueryParams`, and for keeping `Mount` cheap and idempotent.

`Handler.Heartbeat` (default 25s) keeps idle streams from being closed by proxies and is how the
server notices a client has gone. Set it negative to switch it off.

## When a render fails

The error is propagated, never swallowed — but where it surfaces depends on when it happens. On the
first paint the request carries a 500. After an action the POST is a truthful 204 (the action *did*
succeed) and the re-render fails afterwards, on the session's goroutine, where there is no request
left — so it reaches `Handler.Logger` and the page keeps its old markup.

Implement `Fallback` to render something instead:

```go
func (c *Chart) RenderError(_ context.Context, err error) templ.Component {
    return callout.New(callout.Danger) // or whatever you want in its place
}
```

Panics are guarded throughout: a component that panics costs you that render, not the page.

## Things that will bite you

Each of these is a silent failure rather than an error, which is why they are worth knowing up
front.

- **Mutating a component from outside its session races the session's own goroutine.** Everything
  that touches component state — actions, messages, timer ticks — is serialised through one
  goroutine per session, which is why your fields need no locks. Application code coming from
  elsewhere must go through `Base.Do`.
- **`Navigate`, `Replace` and `Emit` are covered by that rule too**, which their names do not
  suggest. The first two run every component's `HandleParams` before returning and the third runs
  the ancestor's `HandleEvent` — on your goroutine, writing component fields. Inside an action, a
  `HandleInfo` or a `Mount` you are already on the session's goroutine and they are fine; from
  anywhere else wrap them: `cmp.Do(func(ctx context.Context) error { return cmp.Navigate(ctx, u) })`.
  `Redirect` is safe anywhere — it only sends a script.
- **Render an `<input>`'s `value=` from committed state, not from something that moves.** The morph
  writes an attribute only when it differs: a value the server never renders leaves the typing
  alone, and one the server changes overwrites it mid-sentence.
- **Give it a stable id** — `shuttle.ID(ctx, input.ID, "email")`. An id that changes per render
  means the element is replaced rather than morphed, which loses the typing, the focus and the
  caret. (`e2e/morph_test.go` holds all three.)
- **A `<textarea>` reaches the same rule by a different route.** It has no `value=` at all — the
  content is the element's text — so the morph compares the *default* value, which is that text and
  which typing never changes, and assigns the property when it differs. Same advice as an input:
  render it from committed state. Both directions are covered in `e2e/morph_test.go`.
- **Streamed items must render `id="<Stream.ItemID(key)>"`.** Shuttle checks and errors if not,
  because the alternative is an item that can never be replaced or removed.
- **Duplicate element ids silently degrade morphing.** Datastar reports nothing at all; set
  `Handler.Debug = true` and shuttle will warn, or call `shuttle.DuplicateIDs` in your own tests.
- **Signal names must be plain identifiers.** Datastar reads dots as path separators and camel-cases
  hyphens. Shuttle fails the render rather than namespacing it wrongly.

## Sessions, identity and abuse

**The session id is a capability.** It is 128 bits from `crypto/rand`, it is embedded in the page,
and it is the only thing standing between a stranger and that page's actions. Two consequences
follow, and they are why shuttle looks the way it does here.

It travels in the `Shuttle-Session` header, never in a URL — a URL is the most copied string in a
system, recorded by access logs, proxies and APM spans before your code sees it. For the same
reason it is never logged: `Session.Tag()` is a short label minted separately from the id, so a log
line can tell two sessions apart without naming either. Log it beside your own identifiers.

And **there is no ambient credential**, which is what makes CSRF not apply: nothing is attached
automatically by the browser, so a forged cross-site request carries no session and can do nothing.
If you add cookie authentication of your own, do not put the session id in it — you would be
reintroducing exactly the ambient authority this avoids. Use `SameSite=Lax` on your own cookies;
shuttle additionally rejects a cross-origin POST outright, and `Handler.CheckOrigin` overrides that
rule when a page is legitimately served from somewhere else.

**Logging out has to reach the page.** A session outlives the request that made it, holding the
component tree and whatever identity it captured at mount, and nothing about a cookie expiring
reaches that:

```go
func (d *Dashboard) Mount(ctx context.Context, _ shuttle.Params) error {
    user := auth.From(ctx)          // your middleware put it there
    d.User = user                   // copy it: async work gets a background context
    d.Session().SetOwner(user.ID)
    return nil
}

// in your logout handler
handler.CloseOwner(user.ID)         // every tab they left open
```

Closing ends the session and its stream; the page reconnects, finds nothing, and reloads — back
through your middleware, which is now the thing saying no. `Handler.Close(sid)` does one page.

**Every request a page makes draws on a per-session budget** — 10/second with a burst of 40 by
default, `RequestRate` and `RequestBurst` to change it, a negative rate to turn it off. It refills
continuously rather than resetting, so a page open all day is in the same position as one just
opened, and it is checked before anything is decoded or queued. It bounds what *one page* can do to
your database, your third-party APIs and, through pub/sub, everyone else's session. It is not a
defence against an anonymous flood: a session id is free to anyone who loads the page, so put a
per-IP limit in front of the page route as well.

Actions carry at most `MaxSignalBytes` (64 KiB) of JSON. Internal errors never reach the client —
the response gets the status, the log gets the cause — so read your logs when a component fails.

## What works

Built and tested: sessions and transport, deterministic ids and per-instance signal namespacing,
forms with the change/submit split, nested components with scoped re-render, pub/sub, timers,
presence, streams, navigation with URL binding, a testing kit, connection recovery, file uploads,
and the live component kit. The parts only a browser can settle — the morph rules, popover
dismissal, reconnection, upload progress — are covered by an end-to-end suite rather than by
assertion here.

Not built yet: per-IP rate limiting, which has to sit in front of the page route since a session
does not exist yet at that point, and infinite scroll.

Server-held state means sticky routing and memory sizing per connected tab. Plan for both before
putting this in front of real traffic.

## Requirements

Go 1.26, [templ](https://templ.guide), and Datastar core v1.0.2 (pinned; the attribute syntax
changed at the 1.0 boundary, so an unpinned bundle can stop understanding this markup).

```bash
go test ./...
```
