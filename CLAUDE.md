# Shuttle

A stateful, server-driven UI layer for Go: Phoenix LiveView / Laravel Livewire developer
experience, built on [Loom](https://github.com/pietjan/loom) for rendering and
[Datastar](https://data-star.dev) for client/server communication.

Loom is the component kit (roughly Flux UI for Go). Shuttle is the stateful layer (roughly
Livewire). They are separate modules on purpose: Loom's components add no script of their own, so
whatever you layer on top is the only JavaScript on the page — Shuttle is that layer, and keeping
it separate means Loom users who don't want it don't pay for it.

Sibling to Loom on disk, but wired through the **published** module, not a `replace` directive:
anyone can `go get` this, and CI builds it the way a consumer would. The consequence to remember
when changing both repos together: loom changes must be pushed (and `go get
github.com/pietjan/loom@latest` run here) before shuttle can see them.

## Settled decisions

These came out of a scoped Q&A and a verified Datastar capability audit. Don't reopen them
casually — each one has consequences threaded through the design.

- **State lives in server memory.** A live session per connected page holds the component tree.
  Not wire-serialized (Livewire) and not signals-only. This is what makes server push, pub/sub and
  presence cheap, and it's why actions can be per-render closures rather than named strings.
- **Full framework** scope: navigation, uploads, pub/sub, presence, streams, testing kit.
- **Authoring**: a Go struct holds state, exported methods are the actions, `Render` returns a
  `templ.Component`.
- **Loom is modified only when the gap is Loom's.** The rule was "not modified", and it held for
  everything through milestone 8: shuttle works through the public seams — `New`/`Node`/`Attr`/`ID`
  per package, and the `data-ui="…"` marker on every component root. It was overturned once, on
  purpose, for `combobox`: Loom's `launcher` doc had already named the filter-as-you-type control as
  missing, and building it *around* Loom instead would have meant shuttle owning a floating panel's
  presentation, which is the one thing this split exists to avoid. A gap in Loom is fixed in Loom;
  a gap in the stateful layer is fixed here.

## The binding shape

Loom gives each package its own option type (`button.Option` is `func(*button.Config)`) and the
constraint that unifies them, `opts.HasCommon`, is `internal/`. So the obvious generic signature is
unavailable. The way around it is to take the package's own `Attr` as a parameter and let inference
work backwards from its result type:

```go
type Attrs[O any] func(key string, val ...string) O

func On[O ~func(T), T any](attr Attrs[O], event, expr string) O
```

```go
button.New(button.Primary, shuttle.On(button.Attr, "click", "@post('/inc')")) { Save }
```

**Bindings must return one option, never a slice.** Go forbids mixing a splat with other arguments,
so `button.New(button.Primary, shuttle.On(...)...)` does not compile. Everything a binding needs
has to arrive as a single value. See `bind.go`.

## Two things that are retrofit-hostile

Get these right before writing component code; neither can be fixed later without breaking
everything downstream.

1. **Deterministic, unique element IDs.** IDs are the morph's reconciliation key. Loom generates
   them from a per-request counter, so a fragment rendered alone gets different IDs than the same
   fragment inside a full page — and a morph then throws away focus and scroll. Shuttle assigns
   explicit IDs via each package's `ID` option (`field` honours a caller-supplied id and follows it
   everywhere). Duplicates silently degrade reconciliation; a missing target drops the patch with
   only a `console.warn`.
2. **Per-instance signal namespacing.** Datastar has one global signal store with no scoping and no
   collision warning, and every action sends *all* signals by default. Namespace per component
   instance and scope each action with `filterSignals`, or every click uploads the whole
   application's client state.

## Datastar

Pinned facts verified against core **v1.0.2** / Go SDK **v1.2.2**
(`github.com/starfederation/datastar-go`). The syntax is colon-delimited — `data-on:click`, not
`data-on-click`; hyphenated forms are pre-1.0. Persistent streams need `openWhenHidden: true` and
`retry: 'always'`, because the defaults abort on a backgrounded tab and don't reconnect after a
clean close.

**There is no load event in v1** — no `data-on:load`, no `data-on-load`. The stream is opened from
`data-init`, which runs when the attribute is initialised. That means it also re-runs whenever the
element carrying it is patched, so it belongs on `<body>`, outside any morph target, or every patch
opens another stream.

`datastar-patch-elements` defaults to `mode: outer` and, with no selector, to the id on the
patched element itself. Giving the component wrapper an id is therefore enough to morph it.

The canonical protocol spec, more precise than the website:
https://github.com/starfederation/datastar/blob/main/sdk/ADR.md

## What exists

Milestones 1–8 of the build order are done — session and transport, ID discipline and signal
namespacing, forms, scoped re-render and nesting, pub/sub and presence and streams, navigation,
the testing kit, and uploads — most of them verified in a real browser as well as in tests. The
live kit is built on top of that, and infinite scroll (`live/feed.go`) is the last item off the
list. `shuttle.go` holds the contract (`Component`, `Mounter`,
`Unmounter`, `Signaller`, `Receiver`, `Informer`, `Base`), `node.go` the component tree and
`Child`, `session.go` the per-page state and its goroutine, `realtime.go` pub/sub, timers and
presence, `broker.go` the `Broker` interface and its in-memory implementation, `action.go` the
closure table and the `OnClick`-style bindings, `ids.go` the naming scheme and `DuplicateIDs`,
`signals.go` the signal namespace, `Bind` and `DecodeSignals`, `forms.go` the change/submit split
and `Validation`, `navigate.go` navigation, `upload.go` uploads, `shim.js` the client JavaScript
with `shim.go` embedding it, `testkit.go`/`assert.go`/`select.go` the testing kit, `registry.go`
session lifetime, `budget.go` the per-session request budget, `clients.go` the per-client page
budget, `handler.go` the HTTP surface, and
`bind.go` the low-level generic binding from the original spike plus `Extra`/`Indicator`.

### One goroutine per session

Everything that touches component state — an action, a pub/sub message, a timer tick, application
code calling `Do` — runs on the session's own goroutine, one item at a time. **This is why a
component's fields need no locks of their own.** It also removes a whole class of deadlock: `Push`
marks the component dirty rather than rendering, and `Do` queues rather than waiting, so both are
safe to call from inside an action, where waiting would be waiting on yourself.

**The mailbox is unbounded, and that is load-bearing rather than laziness.** It was a 64-deep
channel, and the in-memory broker delivers on the publisher's goroutine — so an action publishing
to a topic its own session subscribed to fed its own mailbox from the one goroutine that drains it,
and enough messages wedged the session permanently (two sessions cross-publishing deadlocked the
same way). Client-driven work is bounded by the request budget; what arrives outside a request must
never be able to block. `TestPublishingToYourOwnTopicCannotWedgeTheSession` pins it.

**Session teardown runs on the session's goroutine too.** `close` queues the unmount behind
whatever is in flight and waits for it, so `Unmount` and the cleanup funcs can never race an action
mid-execution — the old shape ran them on the caller's goroutine, which broke the one rule every
component is written against. Consequence: never call `Session.close` from the session's own
goroutine.

Marking dirty rather than queueing renders is also what makes coalescing free — the dirty set can
never hold more than one entry per component, so ten pushes in a row cost one render.

Application code outside the session must mutate through `Base.Do`, not by writing fields directly:
a bare write races the session's own goroutine.

**Three methods extend that rule transitively, and it is not obvious from their names.** `Navigate`
and `Replace` run `HandleParams` on every node before returning, and `Emit` runs the ancestor's
`HandleEvent` - all on whichever goroutine called them, all writing component fields. So they belong
inside an action, a `HandleInfo`, a `Mount` or a `Do` closure, and nowhere else. `Redirect` is the
odd one out and is safe anywhere, because it only enqueues a script.

Running them inline rather than queueing is deliberate and was re-checked: `Session.run` takes one
mailbox item and then renders, so a queued `applyLocation` would land *after* the calling action's
render - stale parameters on screen, two patches to settle - and a queued call cannot return its
error. Enforcing it in the type system was considered and rejected: Go has no goroutine identity, so
the marker would have to be a distinct context type in every component signature, which breaks every
component to catch a mistake the godoc can describe.

This was found by the race detector at about 5% of runs, in tests that called `c.Navigate` and
`c.Replace` directly - the same shape as the stream and timer tests. Three of this repository's own
test files broke the rule, which is the argument for the note living on the methods rather than only
here.

### Multi-node: decided, deliberately not built

The scale-out design was worked through and the conclusion is that only one piece of it is
retrofit-hostile, so only that piece was done. The rest is additive when a deployment actually
needs it, and building it earlier would be speculation:

- **Sessions never migrate.** The tree is closures over live Go values; serializing it is the
  Livewire model this project rejected on day one. Multi-node means routing requests to the owning
  node - LB stickiness, or fly-replay-style forwarding off a session→node map - and failover stays
  what a deploy already is: the page reloads and remounts from its URL.
- **A distributed Broker is a small additive package** (NATS or Redis behind the existing
  interface). Presence across nodes becomes an optional interface the handler type-asserts on the
  broker, so the memory broker keeps today's behaviour untouched.
- **The retrofit-hostile piece is the contract, not the code** - so it is written down now, on
  `Broker` and `Informer`. The in-memory broker accidentally over-promises: synchronous, ordered,
  guaranteed, and able to share live pointers between sessions. Components written against those
  accidental properties break the day a real broker arrives, and every day they aren't documented
  more such components get written. The documented contract is the distributed one: at-most-once,
  unordered across nodes, messages are serializable values, HandleInfo is an invalidation rather
  than the data.

### Pub/sub, timers, presence

`Subscribe` from `Mount`, receive in `HandleInfo` (the `Informer` interface), publish with
`Publish`. The `Broker` interface has an in-memory implementation for a single process; a
multi-node deployment swaps in Redis or NATS and nothing above it changes. The broker delivers on
the publisher's goroutine, so a subscriber hands the message to its own session rather than acting
on it — and it delivers outside its own lock, so answering a message by publishing one is fine.

### Streams

`Base.Stream(name)` patches a container an item at a time — `Append`, `Prepend`, `Replace`,
`Remove`, `Clear` — mapping onto Datastar's patch modes. It exists because server-held state has a
bill attached: a component that re-renders a collection has to *hold* that collection, so a 10k-row
table costs 10k rows per connected tab.

The container carries `data-ignore-morph` (via `Stream.Attrs()`), which is what stops the
component's own re-renders from wiping items it no longer remembers. Verified in a browser: the
component re-rendered and every streamed line survived. The trade is real and worth stating — the
server no longer knows what the list holds, so nothing can rebuild it; a client reconnecting outside
the grace window gets only what the component renders into the container from scratch.

Items must render `id="<Stream.ItemID(key)>"`. Shuttle checks and returns an error if not, because
the alternative is an item that can never be replaced or removed, and Datastar drops a patch with a
missing target with nothing but a `console.warn`.

`Stream.Attrs()` returns those container attributes as a **string**, which is fine for markup a
component writes itself and useless for a Loom component — it takes options, not text. That gap is
why the streams example hand-rolled a `<ul>` for as long as it did. `shuttle.Container(stream,
timeline.Attr)` is the binding form, and the example is a `timeline` now: a log is a sequence of
events, `data-ignore-morph` sits on the Loom component, and the streamed items survive the
component's own re-renders exactly as they did on a bare list. Verified in a browser.

Stream operations are queued in order rather than coalesced — two appends are two items, not one
state superseding another — while component re-renders still coalesce per component.

`Every(d, fn)` runs a timer, torn down with the session. `Join`/`Presence` track who is on a topic;
`PresenceEvent` arrives through `HandleInfo` like any other message. Join subscribes *before* it
announces, so a joiner hears its own arrival — deliberate, so a component can build its view of the
room from events alone with no special case for itself. Presence is per-`Broker` and in-process; a
multi-node roster needs sharing the same way messages are.

### Nesting and scoped re-render

Every component instance is a `node`: its own state, its own action table, its own morph target,
its own children. The per-node action table is what makes a scoped re-render possible — a
session-wide table gets replaced wholesale on every render, so a child re-rendering would
invalidate every action its parent had just handed the client. Action URLs therefore name the
component: `/_shuttle/act/{session}/{node}/{gen}-a{n}`.

`shuttle.Child(ctx, key, factory)` mounts and renders a child. **The key is the whole identity
rule**: the same key keeps the same instance and its state across the parent's re-renders, a
different key mounts fresh, and a key the parent stops rendering is unmounted. `factory` runs only
on first mount, so props captured in it are mount-time props — a parent updating a live child holds
a reference and sets its fields, which costs nothing when the child is a Go struct in the same
process. Children keep a stable path index per key, so a sibling disappearing does not shift
everyone else's ids.

`Base.Emit` sends an event to the nearest ancestor implementing `Receiver`, and re-renders that
ancestor.

### Naming, which is the retrofit-hostile part

Element ids and signal names both come from a component's position in the tree, rendered two ways —
hyphens for the DOM, dots for Datastar's signal paths:

```
root component     id="shuttle-c"          signals under c.
its second child   id="shuttle-c-2"        signals under c.2.
a named element    id="shuttle-c-2-email"  via shuttle.ID(ctx, input.ID, "email")
```

Position, not a counter, because Loom numbers its own ids from a per-render counter — so the same
component rendered alone gets different ids than it had inside the page, and a morph that cannot
match ids throws away focus and scroll. A fresh Loom counter per render keeps Loom's own ids
deterministic but is *not* sufficient once a page holds two components: both would emit
`loom-field-1`. That is what `shuttle.ID` is for, and `Handler.Debug` warns about duplicates,
because Datastar reports none.

Signals are declared by implementing `Signaller`; shuttle emits them on the component root,
namespaced, with `__ifmissing` — without which a re-render resets whatever the user just typed —
and scopes every action with `filterSignals: {include: /^c\./, exclude: /^c\.[0-9]+\./}`. Verified
on the wire: a click on a component with one signal posts exactly `{"c":{"query":"widgets"}}`.

The `exclude` half is why child paths are **numbered rather than named after their keys**.
`filterSignals` matches whole dot-paths, so an include of `/^c\./` catches `c.1.query` as readily as
`c.query`; excluding a numeric segment drops child namespaces precisely while leaving a
component's own object-valued signal (`c.filters.name`) alone.

Three more decisions worth knowing before touching this code:

- **Action ids are `<generation>-a<position>`**, deterministic rather than random. The capability is
  the session id, which is unguessable, so an id only has to be unambiguous — and determinism is
  what lets a test compare two renders byte for byte. The generation prefix is load-bearing:
  position 1 of the next render may be a different button, so an id naming only its position would
  let a click sent against markup a morph has already replaced run the wrong closure. The current
  table plus the last **delivered** one are kept live, so a click racing a morph still runs its own
  closure. Delivered, not merely minted: renders coalesce, so generations can be superseded before
  the stream ships them, and granting those grace would push out the one the client actually holds
  — a click after two undelivered renders was a spurious 404. `take` marks a subtree sent when its
  patch is drained, the page's first paint marks the whole tree, and
  `TestGraceFollowsDeliveryNotMinting` pins it. Note that position follows the order `OnClick` is
  *called*, not document order.
- **One pending render, not a queue.** Every patch carries the region's complete state, so a client
  that can't keep up should skip intermediate renders rather than accumulate them. `Session.Push`
  overwrites the slot and never blocks on the client, which is most of the backpressure problem
  gone for free.
- **The action POST returns 204 and nothing else.** The morph travels down the page's one SSE
  stream, because a single writer is the only ordering primitive available; separate one-shot
  responses have none.

### Pending state: `Indicator`

`shuttle.Indicator("saving")` is an `Extra` on any binding — `OnClick`, `OnSubmit`, `OnEvent`,
`OnChange` — emitting Datastar's `data-indicator`. The signal is true while a request *this element*
started is in flight, which is the one thing the server cannot drive, since the whole point is the
window before it answers. Because `serveAction` blocks on `sess.call`, that window is exactly how
long the action takes on the session goroutine.

**The signal is declared on the component root**, `data-signals:ind.c.saving__ifmissing="false"`,
and that is not tidiness. Datastar creates it when it initialises `data-indicator` — but attributes
are processed in document order, so an expression on the *same element* that reads the signal first
finds nothing there, and that error takes the rest of that element's attributes with it. A button
whose `data-attr:disabled` was passed before its `OnClick` lost the click entirely: no request, no
console message, and a component author would have had to know which option to pass first. Found by
an e2e test that only passed in one of the two orders; `TestIndicatorTracksTheRequest` now runs
both.

Read from the v1.0.2 bundle rather than assumed, because each of these decides whether the feature
is sound:

- the plugin counts `datastar-fetch` events filtered on `detail.el === theElement`, so overlapping
  requests from one element nest correctly rather than racing each other false;
- its **teardown resets the signal to false**, so an element a morph deletes mid-flight cannot
  strand its indicator at true. Verified in a browser by making one action's patch delete a button
  whose own request was still running: the signal went false at the patch and stayed there.

Signals live under **`ind.`, a second root, not under `c.`** The reason is not tidiness: `c.` is the
contract `DecodeSignals` reads, so an indicator there would decode into a component field tagged the
same — and `filterSignals` includes `/^c\./`, so it would ride along in every action payload for a
value the server must never read. `ind.c.1.saving` carries the component's path, so two instances
don't share one, and matches no filter, so it never leaves the browser.

That path is also why `IndicatorRef(ctx, name)` exists, and it is not a convenience: writing
`$ind.c.saving` by hand works for a root component and silently watches the wrong signal once the
same component is mounted as a child. Building `example/site/indicator.go` is what surfaced it —
the example is a child, so it renders `ind.c.1.saving`.

One measured non-issue: the patch and the action's own `finished` event land within ~2ms of each
other in either order (both observed). Neither order leaves a wrong state — worst case a pending
label clears one frame before the new markup arrives.

### Forms

`OnChange` validates as you type, `OnSubmit` commits. `OnChange` listens on `input`, not `change` —
`change` waits for blur, too late to be useful — so it fires per keystroke and its debounce
argument is not optional. `OnSubmit` carries `__prevent`, without which the browser *also* submits
the form normally and navigates the page out from under the session.

`DecodeSignals(ctx, &struct)` is how a handler reads client state: json tags, and anything the
struct does not declare is ignored, so the client can only reach fields the component asked for.
`Validation` pipes straight into `field.Error`, which is a no-op on the empty string precisely so
results pass through unconditionally.

The morph trap here had the symptom right and the cause wrong, and the browser tests separated the
two. What this file used to say was that omitting `value=` clears what was typed. It does not.
There are **two independent mechanisms**, each now pinned by a test in `e2e/morph_test.go`:

- **`value=` decides what the morph writes.** An attribute the server never renders never differs,
  so nothing is written and the typing stands (`TestMorphKeepsWhatWasTyped`); a `value=` the server
  *changes* is written and the property follows it
  (`TestMorphOverwritesWhenTheServerChangesTheValue`). So render `input.Value(...)` from committed
  state — render it from something that moves on its own and you overwrite people mid-sentence.
- **The id decides whether the element is morphed at all.** Give the input a fresh id per render and
  it is replaced rather than morphed, and the typing goes with it — measured, and the failure the
  original note was almost certainly written from (`TestMorphNeedsAStableID`). This is the test the
  "deterministic, unique element IDs" decision above never had.

**`<textarea>` reaches the same contract by a different mechanism**, and the note that used to sit
here — that it "compares the property" — was wrong. It has no value attribute at all: `textarea.Value`
becomes the element's *text child*, so neither rule above is expressible for it. Read from the
v1.0.2 bundle:

```js
else if (r instanceof HTMLTextAreaElement && s instanceof HTMLTextAreaElement) {
    let c = s.value;
    r.defaultValue !== c && (r.value = c, l = true)
}
```

The guard is the live element's **`defaultValue`** — its text child, which typing does not touch —
against the incoming content, and the write goes to `.value` directly. Assigning the property rather
than the text is what sidesteps the dirty-value flag, which would otherwise leave a typed-in
textarea permanently deaf to the server. Afterwards `r.isEqualNode(s) || nn(r, s)` morphs the child
text too, so `defaultValue` converges and the next unchanged render is a no-op — without which every
later re-render would clobber the box again.

So the authoring rule is input's: render the content from committed state.
`TestMorphKeepsWhatWasTypedInATextarea` and `TestMorphOverwritesTheTextareaWhenTheServerChangesIt`
pin all three steps, and both pass in a browser — including the convergence one, which is the step
that decides whether a server can set a textarea once or make it permanently unwritable.

Nothing on the original build order is open now; infinite scroll, the last of it, is `live.Feed`.

### Navigation

Three shapes, and picking the wrong one is the difference between a live page and a page reload:
**patch** (`Navigate`/`Replace` — URL changes, component stays, `HandleParams` runs), and
**redirect** (`Redirect` — full page load, session thrown away).

None of it is free from Datastar: its own `Redirect` is sugar over `window.location`, and
`data-query-string`/`data-replace-url` are Pro. So `history.pushState`/`replaceState` travel down
the stream as `ExecuteScript`, and back/forward comes back up through **the one piece of JavaScript
shuttle ships** — a `popstate` listener that POSTs to `/_shuttle/nav/{sid}`. It arrives via
`Page.Scripts`; a custom `Shell` that drops it loses those buttons.

`Queryer` syncs the root component's state into the query string after every render, with
`replaceState` rather than `pushState` — a re-render is not a navigation, and a filter box should
not fill the back button with a history entry per keystroke. The session tracks the URL it believes
the browser is at, so a render whose state did not change moves nothing, and a `popstate` records
where the browser already went rather than pushing it back.

`HandleParams` also runs on the first render, so a component reading the URL needs only that one
hook rather than duplicating itself into `Mount`.

### The live component kit

`live/` is a separate package: the components Loom's README lists as left out rather than
approximated. `live.Combobox` is filter-as-you-type; `live.Table[T]` is sort/filter/paginate holding
one page; `live.Feed[T]` is infinite scroll, holding one page as well.

A hardening pass over the kit fixed a cluster of edge cases, each with a pinning test:

- **`Column.Cell` takes `ctx`**, so a cell can carry bindings — per-row buttons were impossible
  without it, which was the kit's biggest usability gap.
- **`Table.Param` namespaces the URL keys** (`u-q`, `u-sort`, …). The keys were hard-coded
  literals, so two tables on a page fought over the query string and no app could avoid it.
- **An offset past the end snaps to the real last page** (`?page=99` rendered "491–490 of 15"),
  **hiding the sorted column un-sorts** (the third heading click was the only way back and hiding
  removed it), and a **URL hiding every column un-hides the fallback** so the picker, the URL and
  the screen agree.
- **`HandleParams` writes a changed filter back to the client signal** via `Base.PatchSignal` —
  `__ifmissing` rightly stops renders clobbering typing, but the back button restoring `?q=` must
  reach the box. `PatchSignal` is the core's new explicit, targeted signal write for exactly these
  moments; `Combobox` uses it too, writing the chosen label into its field.
- **`Combobox.Select` presets or clears a selection** (an edit form's combobox starts with a
  value), a nil `Search` stays inert instead of announcing "No matches.", and
  **`data-shuttle-open` carries a search counter, not a boolean** — the shim acts on the attribute
  only when its value changes, which is what lets Escape's client-side dismissal survive later
  patches (a stale `true` re-applied used to pop the panel back open) while each new search still
  reopens it.
- **`Feed` treats an empty page as final whatever `Total` claims** (an overstating source was an
  infinite sentinel loop), advances its cursor by what was actually appended (a mid-stream failure
  no longer double-appends on retry), and a first-page failure retried lands in `first` as well as
  the stream, so `Held()` and a fresh full render stay truthful.
- **`Feed.Prepend` and keyset pagination** arrived together because prepend's duplicate problem is
  what keyset solves. `Prepend(ctx, row)` streams one row to the top under a *negative* key, so it
  can never collide with the appended keys counting up from zero; its contract is that the source
  now serves the row at its head, which is what lets the feed shift an offset source's next fetch
  by the prepend count (or nothing gets served twice) while leaving a keyset cursor alone (it
  points into the sequence below). A source opts into keyset by setting `Page.Next`; the feed
  carries it back as `Query.Cursor`, is not exhausted while it is set - checked *after* the
  empty-page guard, so a source contradicting itself (Next with no rows) still cannot loop - and
  only advances the cursor when the whole page reached the container, so a partial append retries
  the same page. `Table` ignores both fields. `Live.Settle` was exported for exactly these tests:
  work caused outside the kit's helpers (a `Do`-wrapped `Prepend`) needs settling before
  assertions.

**The table holds its shape**, because one that resizes on every sort, page and keystroke throws the
page around under the cursor. Measured before and after on the same five steps — initial, sorted,
page 3, filtered, no matches:

| | width | column widths | pager top |
|---|---|---|---|
| before | 494–560px | moved on every step | jumped 135px |
| after | 880px throughout | identical throughout | fixed |

Three things did it, and each fixes a different cause. `table-fixed` plus an optional
`Column.Width` takes the widths off the content, so which rows this page holds stops deciding how
wide the name column is. A short page is **padded to the page size** with spacer rows —
`aria-hidden`, no separator, no hover — so the last page and a two-row filter result are as tall as
a full one. And the table now renders **even when nothing matches**: the empty message is a row
spanning it rather than a paragraph replacing it, so a filter that matches nothing no longer takes
the column headings away with it.

The fourth cause was the site's, not the kit's: `.demo` hugs its content so a button is
button-sized, which meant the table was as wide as whatever it held. `Example.Wide` opts the table
into `items-stretch`.

**The column picker is `dropdown` with `dropdown.Name`**, an option this work added to Loom for the
same reason `combobox` needed one: a popover's open state lives on the element, so a generated id
means the morph after each toggle replaces the panel and closes it — and hiding two columns would
mean opening the menu twice. Rows are buttons with `aria-pressed`, not checkboxes, because clicking
one changes the page immediately rather than collecting a choice to submit. Which columns are
showing goes in the URL as `hide=` (hidden, not visible, so the common case leaves the URL alone),
and the last visible column renders with no click binding at all *and* is refused server-side —
`Visible()` falls back to the first column, so even `?hide=` everything renders a table.

Finding that bug found another: **adding `combobox-list` to the anchor-positioning rules had knocked
`dropdown-menu` out of them**, so every Loom dropdown had been positioning at the viewport's
top-left since. Restored, and the `anchor-size(width)` rule that was the point of the edit now
applies to the combobox alone — a menu is not its trigger's width.

**Sorting cycles in three**: ascending, descending, then back to the order the source returns rows
in. Two states is the common shortcut and it is a trap — nothing else on a table un-sorts it, so a
click on a heading would be a decision you cannot undo, and the source's own order (often the
meaningful one) would be unreachable for the rest of the session. Switching columns starts that
column's cycle rather than inheriting the last direction.

The third state needs saying in the markup or it reads as nothing having happened, so an unsorted
sortable column now shows a **muted `CaretUpDown`** — which is also the affordance the heading was
missing — and every sortable `<th>` carries `aria-sort` (`ascending`/`descending`/`none`). That
revises the note below: one muted glyph is not the row of competing arrows I was avoiding.

**A sortable heading is a ghost button at the heading's own size**, not a default one: a full button
in every `<th>` reads as a toolbar someone left in the table. The sort direction is `icon.CaretUp`/
`CaretDown` rather than an arrow glyph — Loom ships the set, and a caret that matches every other
caret on the page beats a character that renders differently in every font. It appears only on the
column being sorted, so the heading row is not three competing arrows.

Icons belong wherever a Loom component already styles them: `navlist` sizes them and tints the
current item's (`**:data-[ui=icon]:size-5`, `[&[aria-current=page]_[data-ui=icon]]`), so every
example carries one in the sidebar, named on the `Example` entry rather than mapped by slug in the
renderer. Buttons style them too, which is why Append, Clear, Send and Add a row have one.

**The table's pager is Loom's `pagination`**, not a pair of hand-rolled buttons: numbered pages with
`Gap()` elision, `aria-current` on the current one, and a disabled Prev/Next rendered as an inert
`<span aria-disabled>` — which is the correct markup, since `disabled` does nothing on a link. The
links are real (`?page=3` built from the table's own `QueryParams`, so filter and sort come along)
and bound with `click__prevent`, the same bargain as the site's navigation: copyable address, no
page load. `data-shuttle-page` and `data-shuttle-page-count` survived the move, because they are
what the testing kit and an app's CSS select on. The two share a row inside
`data-shuttle-pager-bar`, count at one end and pager at the other — laid out with an **inline
style**, since a utility class here would only resolve in an app whose Tailwind build scanned this
package, and nothing does: loom's generator points `@source` at loom. Shuttle ships no stylesheet,
so the two rules this needs travel with the markup. `pageWindow` decides which numbers to show and has
its own table test; the count line stays, since "1–5 of 15" is what numbers cannot say and all
there is to say when a source answers `Total < 0`.

**`live.Combobox` renders Loom's `combobox`, which this work added to Loom.** It used to render
`launcher` — a command palette — and the mismatch showed: a palette's list is a panel in the flow,
so the choices pushed the page around instead of floating over it. Loom's own `launcher` doc had
called it: *"the filter-as-you-type form control is a different component and does not exist yet."*

The new component's list carries the **popover attribute**, so the top layer, light dismiss and Esc
are the platform's and the panel escapes clipping ancestors; `Root` sets an `anchor-name`, the list
a matching `position-anchor`, with the absolute fallback already in `cmd/css/loom.css`. All four
verified in a browser: opens on typing, closes on a real outside click, closes on Escape, closes on
a choice.

`combobox.Name` is load-bearing in a way it is not for `launcher`: **a popover's open state lives on
the element**, so a re-render that changes the panel's id closes the list — on every keystroke, for
a component whose whole job is to re-render as you type. There is a test for it in Loom.

Two things had to happen on this side:

- **The shim opens it.** Nothing in Loom runs, so markup cannot say "open". The component renders
  `data-shuttle-open`, and the shim calls `showPopover`/`hidePopover` to match — on
  `datastar-patch-elements` only. Syncing on any patch re-opened a panel the user had dismissed
  every time the 25s heartbeat fired, which is a signal patch.
- **Escape needed doing by hand.** The roving handler `preventDefault`s Escape to return focus to
  the field, which also cancels the popover's own light dismiss, so it now closes the panel first.

Rows are still buttons with no listbox roles, for launcher's reason: arrow keys move real focus, and
`role="option"` on a focusable button describes a widget whose cursor is `aria-activedescendant` and
whose focus never moves. Verified that roving still works with the panel in the top layer.

Keyboard behaviour is **real focus movement**, via `data-shuttle-roving` in the shim, not ARIA roles
painted on rows — following Loom's argument that `role="option"` on a link overrides the link role
and announces something the row does not do. Moving focus keeps Enter native.

Building it found a real gap: **`HandleParams` and `QueryParams` were root-only.** The unit tests
mounted `Table` as the root and passed; as a child it silently rendered nothing, because nothing
ever handed it the URL. `HandleParams` now runs for every node that implements it — at mount and on
every URL change — and `syncURL` merges `QueryParams` from the whole tree. Two components using the
same key would fight over it, which is the app's to avoid.

### Infinite scroll, and the one attribute that is not `data-on:`

`live.Feed[T]` reads the same `Query`/`Page[T]` a `Table` does, so one source backs either. It is
the last item off the original build order, and three things about it are load-bearing.

**`shuttle.OnIntersect` writes `data-on-intersect`, hyphenated and keyless.** Everywhere else in
this package the rule is `data-on:<event>`; here it is not, because Datastar v1 registers
`on-intersect` as a plugin in its own right with `{key: "denied"}` — so `data-on:intersect` is not a
synonym but an error, and an erroring attribute takes the rest of that element's attributes with it.
`OnEvent` and `OnIntersect` now share `onAttr`, which takes the attribute name for exactly this
reason. Free in core v1.0.2, not Pro; verified in the bundle.

**A sentinel fires on a transition, so what re-arms it is its attribute value changing.** An
IntersectionObserver reports entering the viewport, so a first page too short to push the sentinel
off screen would leave a feed stopped with more to give — the classic stall. What prevents it was
read out of the v1.0.2 bundle rather than hoped for: Datastar's mutation observer re-applies a
plugin when its attribute *value* changes, so the plugin is re-applied, a fresh
`IntersectionObserver` observes the element, and `observe` always delivers an initial callback with
the current state. Still on screen, so it fires again, until the viewport is full or no sentinel is
rendered. The bundle applies the new plugin *before* running the old cleanup, so the element is
never unobserved in between.

This used to lean on "every render writes a new `<gen>-a<n>`" — and the generations-on-change
commit quietly broke it: a `more` that changes nothing in the feed's *own* markup (the loaded rows
live in the ignored container, not in the render) kept its bytes, kept its generation, and the
sentinel never re-armed — the feed stalled after exactly one extra page, in the browser, with every
unit test green. The sentinel now carries `data-shuttle-sentinel="<sent>"`, the offset its next
firing will ask for, so the render after each load always differs by at least that. The e2e feed
tests are the ones that catch this class of break; run them first if any of this is touched,
because every other symptom of getting it wrong looks like "the feed sometimes stops".

Two consequences of firing repeatedly. The action has to advance a cursor rather than read one, and
**exhaustion is expressed by not rendering the sentinel** — nothing else stops the asking. So is
failure: a `Load` that errors renders a retry button instead, because a sentinel left on screen
beside a failing source is a request loop bounded only by the session's budget.

**The first page is rendered and the rest is streamed**, which is the shape of the memory argument.
A feed that re-rendered its list would hold the list, so a reader who scrolls costs the server
everything they have seen; `Feed` holds one page — `Held()` — however many the reader has —
`Loaded()`. Rendering the first page rather than streaming it keeps first paint complete, which is
the promise this framework makes and the one a feed is most likely to be the whole of. It is also
why `Reset` exists as a method: the container is `data-ignore-morph`, so markup written into it
after first paint never arrives, and starting over has to clear and re-stream rather than re-render.

Two smaller things. The sentinel's height is an **inline style** as well as a class, for the reason
the pager's layout is: a class only resolves in an app whose Tailwind build scans this package, and
here the failure is a feed that silently never triggers rather than something that merely looks
wrong. And in `example/site/feed.go` the count is rendered *after* the feed and moved above it with
`order-first` — a child mounts as it renders, so a count written before it reads a component that
has not loaded anything yet.

**Building it found two bugs in the testing kit**, both of which made any streaming component
untestable without saying so. `settle` applied every patch as an outer replace, ignoring
`patch.mode` — but a stream's append names the *container* and carries one item, so it swapped the
container for the item and every later assertion was about a document the browser never had. And
the root's re-render replaced the markup wholesale, which is precisely what `data-ignore-morph`
exists to prevent: items streamed before it disappeared. `applyPatch` honours the mode now and
`keepIgnored` carries ignored containers across a morph, following the bundle's rule — skip the pair
outright when old and new both carry the attribute, and skip anything inside one.
`TestTheKitModelsAStreamTheWayTheBrowserDoes` fails without either half. `Live.Intersect` is the way
in for the binding, since a test cannot reach it by naming a DOM event.

### Uploads

`upload.go` — their own multipart request, because the stream is one-way and cannot carry bytes up,
and driven by `XMLHttpRequest` because **`fetch` reports no upload progress at all** (Datastar's own
Pro feature for it was withdrawn over browser support).

Files stream to temp files via `MultipartReader` rather than `ParseMultipartForm`: a component
already costs memory per connected tab, so buffering whole uploads turns one large file into an
outage. They are deleted once `HandleUpload` returns.

Every limit is checked server-side even though the picker was told the same rules — `accept` and any
client check are courtesies. `UploadedFile.Save` reduces the client's filename to a base and strips
separators, because `../../etc/passwd` is exactly what an upload endpoint receives; there is a test
for it.

Progress lands on the input as `data-shuttle-uploading`, `data-shuttle-progress` and a
`--shuttle-progress` custom property, so a progress bar is CSS rather than a round trip per percent.

That is only half of what an upload looks like, and `example/site/upload.go` now shows both: the
transfer, which only the browser can see, and what the server does with the bytes afterwards, which
only the server can see. The second is ordinary state going down the stream, and its fake work has
to run **on its own goroutine** — `HandleUpload` is called inside `sess.call`, so sleeping there
stalls every component on the page — mutating through `Do`, which is also the loop's exit: it
returns `ErrNotMounted` once the component is gone. One `progress` component serves both halves,
because the CSS already says which source wins while `data-shuttle-uploading` is present.

### Capabilities, budgets and what the client is told

The session id is the client's capability for a whole page, and most of this section is one idea
applied repeatedly: **a capability must not be copyable by accident**, and the places it leaks are
never the dramatic ones.

- **It travels in `Shuttle-Session`, not the path.** All four transport routes used to name it —
  `/act/{sid}/…` — which puts it in every access log, proxy log and APM span before any of this code
  runs. A header also cannot be set cross-origin without a preflight nothing here answers. Datastar
  supports a `headers` option on `@get` as readily as `@post` (verified in the v1.0.2 bundle: the
  plugin factory destructures `headers` before branching on method), and the shim carries it on the
  two requests it makes itself.
- **`Session.Tag` exists so the id is never logged.** Six bytes, minted separately rather than
  derived: a truncated hash is still a function of the capability, which turns "is this safe?" into
  an argument about how many characters. `redactPath` was the first attempt and got deleted — it
  only fixed the half of the leak this process controls, and a dead function with a security-sounding
  name is worse than none.
- **Presence was the serious one.** `Member.Session` carried the id to every subscriber on a topic,
  so a component rendering a roster — the thing rosters are for — would print every other page's
  capability into this page's markup. It is `Member.Tag` now, which is a rename on purpose: every
  use site fails to compile. This made tag uniqueness load-bearing, so `newTag` falls back to a
  counter rather than `""` when the entropy pool cannot be read.
- **Internal errors do not reach the client.** `publicMessage` passes through a fixed list of
  *refusals* — this package's own sentinels naming a rule the client broke — and answers everything
  else with the status text. `ErrPanic` wraps the panic value, so the previous behaviour echoed
  whatever was in scope into the response body.

**CSRF does not apply, and that is a property rather than an omission**: nothing is attached by the
browser automatically, so a forged cross-site POST carries no session. The README used to list it as
unbuilt, which undersold it. The origin check on the three changing routes is defence in depth for
the app that adds cookie auth of its own and hands these routes ambient authority again;
`CheckOrigin` follows gorilla/websocket's rule, host-only and permitting an absent `Origin`, since a
POST without one did not come from a page.

**Uploads check the bytes.** `Accept` used to be matched against the multipart part's declared
content type, which is a string the client writes — so an executable calling itself `image/png`
passed, and the check was a formality dressed as a control. `receive` sniffs the first 512 bytes
before writing anything, and the declared type is recorded as `DeclaredType` and never acted on in
the default path. The declared pre-check was removed rather than kept alongside: once the bytes
decide, a label check can only reject genuine files that a browser happened to send as
`application/octet-stream`. Detection is signature-based, hence the `text/*` allowance (everything
textual detects as `text/plain`) and `TrustDeclaredType` for container formats — which
**substitutes** the declared type into the Accept check rather than waiving it. It was found to be
a silent no-op (any bytes under any label passed a spec that set it); now
`TestTrustDeclaredTypeStillChecksTheDeclaration` pins that the label is at least required to match.

Two tests were passing for the wrong reason and only failed once the check became real — both sent
the *string* `"MZ"` or `"not text at all"`, which is text, and were refused solely because the client
labelled them otherwise. The e2e one could not be found until Docker was available, which is the
argument for running that suite before believing this kind of change.

**The request budget is per session**, a token bucket on `Session` under its own mutex, checked in
`budgeted` before the handler so a refusal costs a map lookup and never reaches the session's
goroutine. All three POST routes draw on it, because they all queue onto that one goroutine.

A bucket rather than a window because a window has to be reset and the reset is the hole. The
property that matters for a framework whose pages stay open all day: **elapsed time is not credit** —
refill stops at the cap, so a session idle for a day is full, not owed a day's worth. Refill is lazy,
so ten thousand idle sessions cost no goroutines. Defaults are 10/s with a burst of 40 — sized
against what a page actually does, and it is also what bounds the now-unbounded mailbox on the
client-driven side.

It bounds what one page can do — to your database, to a metered API, and through pub/sub to
everyone else's session, since one `Publish` costs every subscriber a render. What it cannot bound
is how many pages there are, because a session id is free to anyone who loads the page: a caller
refused by one bucket takes another session. That is the second limiter's job.

**The page budget is per client**, in `clients.go`, wrapping the page route as `metered`. Same
bucket type, an order of magnitude smaller — 1/s with a burst of 10, because a page load is a human
action — and it closes the hole the session budget structurally cannot. The registry's cap of 10,000
already stopped a page flood from exhausting memory; what it never gave was *fairness*, and one
client holding all ten thousand slots is a denial of service assembled entirely from legitimate
requests.

Three things about it are load-bearing:

- **Who the client is, is a hook.** `Handler.ClientIP` defaults to the connection's address, which
  is right when the handler is exposed directly and is a **footgun behind a proxy** — every request
  appears to come from the proxy, so the whole site shares one bucket and `PageRate` becomes a limit
  on the server. `ForwardedClientIP(hops)` is the answer, and it counts `X-Forwarded-For` **from the
  end**: a proxy appends, so the entries nearest the end are the trustworthy ones and everything
  before them is whatever the client sent. Reading the leftmost entry — the usual mistake — is not a
  limiter at all, since the client then picks its own bucket. **hops must be exact**: too low
  charges the proxy, which degrades safely, but too high reads a position the client controls —
  the header's total length is theirs too, so padding it puts their chosen value at that position
  and the limiter opens. The chosen entry must at least parse as an address (arbitrary strings
  never become bucket keys), but that confines forgery rather than preventing it. The earlier claim
  here that a wrong count "never opens" was wrong in the too-high direction.
- **IPv6 is charged per /64.** The smallest allocation anyone gets is a /64, so a limit keyed on the
  full address is a limit per attempt against eighteen quintillion keys. `narrow` passes through
  anything that is not an address, which is what lets `ClientIP` return an account id.
- **Eviction is exact, not approximate.** A map keyed by something the client chooses is a
  memory-exhaustion primitive unless something empties it, so buckets that have refilled to the cap
  are swept — and dropping one is provably free, because `take` starts a fresh bucket full, so a
  refilled bucket holds no information. `TestAForgottenClientIsWhereItWouldHaveBeen` pins that
  equivalence. Only clients that spent recently are remembered, which is a set the rate bounds.

Neither limiter answers a distributed flood, and neither pretends to; a negative rate turns either
off for a deployment whose edge already does this, since two limiters disagreeing is worse than one.

**The limiter forced a shim fix, and it was a pre-existing bug.** Every fetch Datastar makes
dispatches `datastar-fetch` on `document`, and the shim mapped any `error` to the reconnect state —
so a 429 on a click would have said "connection lost" over a working stream, as would any failing
action. `detail.el` is the discriminator (the same field the indicator plugin filters on): the
stream's is whatever carries `data-init`, a click's is the button. Failures are now gated on that;
successes still count as proof of life whatever started them. `TestConnectionStateReachesThePage`
passes, which is the check that mattered, because the failure mode of getting this wrong is a page
that silently stops reporting real disconnections.

**CSP is `Handler.Nonce`, and the stream half is the part to remember.** The app's middleware
mints the per-request nonce and the hook reads it back; shuttle stamps the inline shim, the
Datastar module tag and `Page.Nonce` for custom shells. The non-obvious half: scripts the stream
injects later - navigation's history calls, redirects, `tellToReload` - must carry the nonce of
the page they land in, so the session remembers it for its whole life, and the reload path (which
has no session by definition) gets it back via the `Shuttle-Nonce` header the attach expression
sends. Both the hook's value and that header are validated against `nonceRE` - one alphabet, one
length cap - before becoming markup, because escaping a value with exactly one legitimate shape is
the wrong tool. A nonce cannot fix `'unsafe-eval'`: Datastar compiles `data-*` expressions with
the Function constructor (verified in the v1.0.2 bundle), so that stays in `script-src` either
way. `TestNonceReachesEveryScript` and `TestNonceIsValidatedBeforeItIsMarkup` pin all of it.

**Logging out needs `Handler.Close`/`CloseOwner`.** A session outlives the request that made it,
holding the identity it captured at mount, and nothing about a cookie expiring reaches it. The
recovery path needed no new machinery: closing ends the stream, the client reconnects, the registry
has nothing, and `tellToReload` sends the page back through the app's middleware. `SetOwner` labels
the session rather than the component, because component fields belong to the session's goroutine and
`CloseOwner` runs on a request's. `CloseOwner("")` matches nothing, or one component forgetting to
call `SetOwner` would make every logout global.

**Observability is `Handler.Stats()` and nothing else** - a snapshot struct in `metrics.go`: two
gauges read from the registry, cumulative counters kept as atomics in `counters`, shared with every
session so session-side events (patches, backlog drops, contained panics, takeovers) land in it.
No metrics dependency, deliberately: the app polls the snapshot into whatever exporter it already
runs. `TestStatsCountTheTraffic` drives one of everything through a handler and asserts each
counter moved - a counter nothing increments is worse than none, because a dashboard shows it flat
and calls that health. When adding a counter, add its increment to that test.

**Shutting the process down needs `Handler.Shutdown(ctx)`.** It ends every session and waits for
their teardowns, bounded by ctx, and refuses new sessions afterwards (`ErrShutdown` → 503); a
connected page's stream ends and its reconnect lands in `tellToReload`, the same hand-off a deploy
already relies on. Call it after `http.Server.Shutdown`, the way that type orders Close after
Shutdown. The waiting split matters and is why `beginClose`/`close` are two methods: eviction and
`Handler.Close` must *not* wait (Close is documented safe from inside an action, where waiting on
the session's own goroutine is waiting on yourself), while Shutdown and the testing kit must.

### Unmount tears down, and nothing patches a dead component

Found from a console error - `PatchElementsNoTargetsFound` - once navigation started unmounting
components for real. Everything a component starts must stop when *it* is unmounted, not when the
session ends:

- `Subscribe`, `Every` and `Join` register their teardown on the **node**, not the session. On the
  session they outlive the component, so a child's timer keeps firing after its key stops being
  rendered.
- `markDirty` refuses a component that is no longer mounted, which catches a message or a tick that
  was already in flight.
- **`Stream.send` needs its own check.** Stream operations never go through the dirty set, so
  guarding renders alone misses them - a timer tick queued as its component was unmounted still
  patched a container that had left the page. That was the actual leak, and it took a browser to
  see it: the server sends the patch happily and Datastar drops it with a console warning nobody on
  the server ever hears.

The lesson worth keeping: a patch aimed at a missing element is **silent server-side**. Anything
that can emit a patch outside a render needs a mounted check of its own.

### One session per section, not per page

`Handler.Subtree` makes one handler own every path under its mount, and the component reads
`Base.Path()` to decide what to render. Links become `Navigate` actions rather than `href`s, so the
path is pushed from the server and the page never reloads.

This is what makes the connection budget survivable in practice: a page load is a session and a
stream, so nine examples behind href links is nine of them. The examples site is now one handler
mounted at `/e/` with `Subtree`, and browsing all nine three times over holds **one** stream -
measured, against 6 connections and a stalled page after four navigations before.

Deep links still work, because a subtree handler serves the path directly and `HandleParams` runs on
the first render.

### The connection budget: serve over HTTP/2

Measured, not assumed, after pages stopped loading while several examples were open.

Datastar streams over `fetch`, not `EventSource`, so **every connected page holds one of the
browser's ~6 connections per origin**, shared across tabs. Five or six live pages and the next one
never loads — curl gets a 200 instantly while the browser hangs, because the limit is entirely
client-side. One client with six concurrent streams: 6 TCP connections over HTTP/1.1, **1 over
HTTP/2**. Browsers only speak HTTP/2 over TLS.

`Handler.OpenWhenHidden` stays **true** by default, which is
[Datastar's own guidance](https://data-star.dev/how_tos/prevent_sse_connections_closing). It was
briefly flipped to false to reclaim connections, which was a bad trade: with it off a hidden tab's
stream closes, `Grace` evicts the session half a minute later, and returning to the tab reloads the
page and loses its state. It is a `*bool` so false stays expressible, and `Handler.Grace` is now
configurable for a deployment that does make that trade. **HTTP/2 is the fix; nothing else is.**

### Connection state and recovery

`shim.js` is the only JavaScript shuttle ships: the `popstate` listener and a connection watcher.
It is a real `.js` file, `//go:embed`ed by `shim.go` — so it is JavaScript to an editor, a
formatter and a linter, and nothing in it is escaped for Go's sake (the old string template needed
a doubled `%%` for one CSS percentage). What varies per session is one generated call appended
after it, `shuttleShim({nav, up})`, inside a `type="module"` script — which is what keeps the
function itself out of the global scope, so a page carrying the shim gains no names of its own.

Three things about Datastar's `datastar-fetch` event were verified in a browser rather than
assumed, and each would otherwise have made the watcher silently do nothing:

- it is dispatched on **`document` with `bubbles: false`**, so a listener on `window` never fires;
- **`detail.url` does not exist**, so the page's own stream cannot be told apart from an action's
  fetch that way;
- **`detail.type` also carries the SSE event names** (`datastar-patch-elements`, `-signals`), not
  just the fetch lifecycle.

That last one is the mechanism: a patch event can only have arrived down the stream, so it proves
the stream is alive — and the server's heartbeat sends one on a timer, so the proof keeps arriving
on an idle page. State lands on `<html>` as `data-shuttle-state` and one of
`shuttle-connecting`/`-connected`/`-reconnecting`/`-dead`, for the app to style.

**A stream request for an unknown session gets a stream carrying a reload, not a 404.** This is the
important one: after a restart every open page holds a session the new process has never heard of,
and Datastar would retry that 404 until it gave up for good — so without this, every deploy
permanently breaks every open page. Verified by restarting under a live page: it reloaded itself
and came back with its state intact, because the state was in the URL. That is what makes cheap,
idempotent `Mount` a real constraint rather than advice.

`Handler.Heartbeat` (default 25s) writes an empty signal patch on an idle stream. It keeps
intermediaries from closing the connection, and a write is the only way this end learns the client
has gone.

**A second stream displaces the first rather than getting a 409.** The refusal needed liveness
detection to be safe: a client that vanished without closing held the slot until a heartbeat write
failed — up to a whole heartbeat, or *forever* with the heartbeat disabled, which bricked
reconnection outright. The session id is a capability, so a second holder is almost always the same
client's newer attempt; the newest wins, the displaced stream is cancelled, and its teardown does
not start the eviction clock on a session someone else now holds (`streamSlot` tokens are what make
that safe). `TestSecondStreamTakesOver` replaced the 409 test.

**A stream also greets on connect** — the same empty patch, once, unconditionally. A client cannot
tell a live stream from a stalled one until bytes arrive, and Datastar's retry is silent, so a page
that had just reconnected went on showing "connection lost" over a working connection until the next
heartbeat: **up to 25 seconds**. The e2e test found it by waiting 20s and failing, then passing at
27. Unconditional because it is proof of life rather than a heartbeat, so `Heartbeat = -1` does not
turn it off — and it gets bytes past an intermediary that would otherwise sit on the response.

### Testing kit

`shuttle.Test(tb, cmp)` in `testkit.go`, with `assert.go` and a small selector engine in
`select.go`. It lives in the main package because the plan's sketch put it there, and takes a `TB`
interface rather than importing `testing` into library code.

Two properties worth preserving: it asserts on **the markup the session actually pushed**, not on a
render performed for the test — so a component whose action pushes nothing fails in a test the same
way it fails in a page. And it settles by queueing an empty item behind the work, since the mailbox
is FIFO; without that an assertion races the render it is checking.

A third, added late and easy to undo by accident: **patches are applied by mode, and
`data-ignore-morph` is honoured**. See the infinite scroll section — a kit that takes the shortcut
here reports that the browser lost things it did not lose.

The selector engine handles tags, `#id`, `.class`, `[attr]`, `[attr=value]` and descendants. No
combinators, no pseudo-classes — a component test needing more is usually asserting on the wrong
thing.

### Errors and where they surface

A component's `Render` error is propagated, never swallowed — but where it *lands* depends on the
path, and this changed when the session goroutine arrived:

| render fails | client sees | you see |
|---|---|---|
| first paint | 500, session removed | logged |
| after an action | 204 — the action itself succeeded | logged |
| async (pub/sub, timer, `Do`) | nothing | logged |

Rows two and three are inherent: the re-render happens after the request is gone. `Fallback`
(`RenderError`) is the opt-in boundary — implement it and the component renders its own error
markup instead of leaving the page stale. The body renders into its own buffer first, so a
component that fails halfway never ships the half it managed.

**Everything on the session goroutine is panic-guarded**, and that is not a nicety: one goroutine
runs all component work, so a panic escaping a render used to take it with it and leave a zombie
session whose mailbox was never drained again — every later click would hang rather than fail.
`Session.safely` wraps the loop, `call` always sends a result even on panic, and `Render` itself is
guarded both when building the component and when rendering it.

### Using it in an app

`README.md` is the user-facing entry point — keep it honest about what is and isn't built.

`Handler.Prefix` is where the handler is mounted (`/dashboard`); both the transport routes and the
URLs rendered into the page are built from it, and the routes are built lazily on first request so
`Prefix` can be set after `New`. `Handler.Shell` replaces the built-in document for apps that need
their own `<html>`/`<body>` attributes; `DefaultShell` remains the fallback.

The page is served at exactly the mount point, not as a catch-all: a catch-all mints a session per
stray GET (`/favicon.ico` was doing it), and each costs a component tree.

## Examples

`example/counter` is deliberately minimal — one component, one action, no configuration — and is
what the README's first snippet shows. `example/site` is the examples site: one focused example per
feature, each its own `shuttle.Handler` mounted under `/e/<slug>/`, which also exercises
`Handler.Prefix` for real.

**The chrome is Loom's** — `sidebar`, `navlist`, `heading`, `text`, `callout`, `code`, `progress` —
because a site demonstrating the stateful layer over Loom that hand-wrote its own navigation would
be arguing against itself. It also puts real weight on the seams: nav links are `navlist.Item`, a
genuine `<a href>` that a fresh load serves, bound with `click__prevent` so an ordinary click is a
`Navigate` action instead of a page load; the sidebar is a popover, so the mobile drawer is the
platform's and not session state.

**The build sources this repository too, so shuttle may write Tailwind classes.** `cmd/css` writes
`loom.css` — Tailwind, the theme, Loom's structural CSS, and an `@source` at Loom's module
directory. `example/site/css/input.css` is hand-written next to it: `@import "./loom.css"` plus
`@source "../../.."`. Both halves matter, and the failure modes are quiet — `@source`-ing a
stylesheet instead of importing it compiles a sheet with *nothing* in it, because a CSS file read as
content is just a list of words to scan.

So the site's chrome is classes now, not hand-written CSS, and so is `live/`'s markup. **That is a
requirement on every app using the kit**: a Tailwind build that sources only Loom compiles a page
whose pager has no layout. It is the same bargain Loom already asks for, one directory wider, and
the README says so.

**The site compiles its own stylesheet** — `make site/css`, which `make run` depends on — rather
than borrowing the one Loom's repository builds. Borrowing was a shortcut and it does not hold up:
that sheet is built for Loom's *site*, whose `site.css` redefines the variant as `@custom-variant
dark (&:where(.dark, .dark *))` so a theme toggle can override the OS. Its components therefore
stay light until something sets `.dark` on `<html>`, which this site has no reason to have — and
two attempts to paper over it from the page's own CSS both failed in a browser (a dark canvas under
light components with an invisible heading; then a light page around dark components once `.dark`
was set). Compiled here, the sheet keeps Tailwind's default media-query `dark:`, the page follows
the OS, and `siteCSS` is four rules again.

The lesson generalises past this repo: **a compiled stylesheet carries the choices of the project
that built it**, so an app using Loom runs `cmd/css` itself. It is two commands.

Without a sheet nothing is styled — the components, the chrome and the page's own layout, since all
three are now classes — and `<aside popover>` is hidden outright, so the site says so on startup
rather than leaving it to be guessed.

`siteCSS` is down to **one rule**: the upload bar's fill has to out-specify the inline width Loom's
progress-bar carries, and `!important` from outside the component is the honest way to say that.
Everything else is a class on the element that needed it, including the reconnect banner, which
reads the shim's state class with an arbitrary variant (`[.shuttle-reconnecting_&]:block`).

Chroma is not cheap and an embedded source file cannot change, so `prerender` highlights each one
once, before the first request — after that the map is read from every session's goroutine. Using
`code` does pull chroma into shuttle's own `go.mod` as an indirect dependency; Loom already required
it, so the module graph is unchanged and nothing importing shuttle compiles it.

Its shell is a worked example of `Handler.Shell`: it owes the document `Page.Attach` on `<body>`
and `Page.Scripts` somewhere, or the page will not connect and its back button will not work. It
also styles `.shuttle-reconnecting` into a banner and drives the upload progress bar from
`--shuttle-progress`, which is the intended way to use both.

Sources are shown with `//go:embed *.go`, so an example and the code beneath it cannot drift.

**Reach for a Loom component before writing markup.** The site drifted into hand-rolled `<ul>`,
`<li>`, `<details>` and inline-styled `<span>`s that Loom already had answers for: `accordion` for
the source panel (native `<details>`, so opening it costs no script and survives a morph),
`separator` for the rule above it, `stat` for the counter's number, `badge` for a presence count or
a tick tally, `description` for the facts about an uploaded file, `timeline` for a streamed log,
`fileupload` for the picker. What is left in raw markup is layout — the page grid, the `.rows`
boxes that space things — which is the split Loom asks for: it styles components, not your
document.
**One example per file** — `counter.go`, `form.go`, `navigation.go`, `stream.go`, `feed.go` and the
rest —
because the source panel shows a whole file: two examples sharing one meant each showed the other's
code as well. `kit.go` is the deliberate exception, since the combobox and the table work over the
same directory and seeing it is the point.

## The end-to-end tests

`e2e/` holds what only a browser can answer, behind an `e2e` build tag so `go test ./...` neither
slows down nor needs Docker:

```bash
make test/e2e
```

Same shape as toto's: `docker run` the official playwright image as a browser server, point
`SHUTTLE_E2E_WS` at it, run `go test -tags e2e ./e2e`, stop the container whichever way the tests
went. Without the env var they skip rather than fail. The handler runs in-process on the host.

What they cover: reconnect keeping its session and the deploy case reloading the page, server push
reaching a second page, presence counting both and dropping one, the
indicator through a slow action *in both option orders*, the back button through the popstate shim,
the connection state on `<html>`, a click on one child leaving its sibling's DOM node untouched, an
unmounted component going quiet, the combobox popover's four dismissals and its roving focus,
streamed items surviving a re-render, the two morph rules and the id that decides which applies,
`<textarea>`'s defaultValue guard and its convergence, the
table holding its shape across sort/page/filter/empty, the sort cycle, the column picker staying
open, a feed loading on scroll and walking itself past a first page too short to fill the window,
and an upload with its refusal - which is now a refusal on the file's bytes, and caught a fixture
that had been passing on the client's label alone.

**Layout tests need the stylesheet.** Behaviour is behaviour unstyled, but `table-fixed` and `w-52`
are just words without the compiled sheet - the first version of the shape test measured an unstyled
page and reported nonsense. `styled` links it, `needsStylesheet` skips when it is missing, and
`make test/e2e` builds it first.

Four things about the setup, each of which cost a run to find:

- **The image ships the browsers, not the npm package.** 1.2GB of chromium/firefox/webkit at
  `PLAYWRIGHT_BROWSERS_PATH`, and no `playwright` anywhere on the filesystem - it is a base image
  for a project that brings its own. That is why the target's `npx` line pins a version: a bare
  `npx playwright` fetches whatever is newest and greets the SDK with a protocol mismatch.
- **That version must match what playwright-go pins** (`run.go: playwrightCliVersion`).
- **Every playwright-go after v0.6000.0 declares the pre-rename module path**
  (`github.com/mxschmitt/playwright-go`), so that is the path this module requires it under - the
  GitHub redirect makes the fetch work, and requiring the community path fails to resolve. The
  upgrade was forced, not chosen: the old driver-zip CDN answers 400 for every version (staying on
  v0.6000.0 means the driver can never be downloaded again - only machines with a warm
  `~/.cache/ms-playwright-go` kept working, which is how it looked fine locally while CI's cold
  cache failed), and v0.6201+ assembles the driver from the playwright-core npm package instead.
  Driver 1.62.1 is where the image tag comes from. toto has the same dead setup and has not
  noticed, because its CI does not run its browser suite.
- **The test server listens on `0.0.0.0`.** The browser is in a container and comes back through
  `host.docker.internal`; a `httptest.Server` on the loopback is invisible to it, and presents as a
  page that never loads.

**These replace prose, and they earned it on the first run**: the morph note in this file was wrong
and a test said so. What belongs here is why a decision was made; what belongs there is whether it
still holds.

## Go 1.27 testing conventions

The unit suites lean on two things Go 1.27 made available, and both carry rules:

- **`testing/synctest` for anything time-driven.** Timer, teardown-race and lifetime tests are
  wrapped in `synctest.Test`; the sleeps inside are real durations on a fake clock, so "wait 80ms
  to prove the ticker stopped" costs nothing and never flakes.
  `TestSessionLifetimeRunsOnItsDocumentedSchedule` runs the attach window and grace verbatim - 30
  real seconds each - in milliseconds, and `TestTheKitRunsUnderSynctest` pins that the testing kit
  works inside a bubble, which closes the old "`Every` is untestable without wall-clock sleeps"
  gap. The rule inside a bubble: everything must block *durably* (channels, sleeps, the fake
  network) - a goroutine stuck on a bare mutex freezes the clock and the test hangs.
- **`httptest.NewTestServer` everywhere a browser is not required.** Every unit-test server runs
  on the in-memory fake network: no TCP, no ports, synctest-compatible. Two sharp edges, both hit:
  `srv.URL` is only materialised by the first `Client()` call, so helpers call `Client()` before
  building a request from it; and Go 1.27 auto-drains unread HTTP/1 response bodies on close,
  which on a never-ending SSE stream is a drain that never returns - `openStream` cancels its
  request context before closing the body, or (under synctest) the drain's mutex wait freezes the
  bubble.

## CI

`.github/workflows/ci.yml`, same shape as toto's: unit tests with `-race` on ubuntu and windows,
lint through the tools module (`go tool -modfile=tools/go.mod golangci-lint run`, the same binary
`make audit` uses — a bare `golangci-lint` on PATH may be the wrong major version), and the full
browser suite via `make test/e2e`. The e2e job gates merges on purpose: it is the only layer that
catches morph/popover/IntersectionObserver breaks, and it has caught ones the unit suite waved
through.

## Commands

```bash
go test ./...
```

```bash
go run ./example/counter   # the minimal one
make run                   # the examples site, stylesheet compiled first
make site/css              # just the stylesheet
make test/e2e              # the browser suite, in the playwright container
```

Loom's own suites must keep passing untouched — `arch_test.go`, `contract_test.go`, `tier_*_test.go`
in the sibling repo.
