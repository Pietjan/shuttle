package shuttle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
)

// Navigation has three shapes, and picking the wrong one is the difference
// between a live page and a page reload:
//
//   - patch: the URL changes, the component stays, [ParamsHandler] runs.
//     Filters, sorting, pagination.
//   - navigate: a different URL on the same session and the same stream.
//   - redirect: a full page load, which throws the session away.
//
// None of them is free from Datastar. Its Redirect is sugar over
// window.location - a full page load - and the attributes that would do the
// rest, data-query-string and data-replace-url, are paid. So the history
// calls travel down the stream as scripts, and back/forward comes back up
// through a small shim.

// ParamsHandler is implemented by components whose state depends on the
// URL. HandleParams runs when the URL changes without the component being
// remounted - a filter applied, the back button pressed - and on a
// component that also implements [Mounter], after Mount.
type ParamsHandler interface {
	HandleParams(ctx context.Context, p Params) error
}

// Queryer is implemented by components that keep some of their state in the
// URL, so a filtered or sorted view can be shared and bookmarked.
//
// The values it returns become the page's query string after every render,
// via history.replaceState - no page load, no new history entry. Returning
// nil clears the query string.
//
// Only the root component's URL state is synced; a child does not own the
// page's address.
type Queryer interface {
	QueryParams() url.Values
}

// Navigate changes the page's URL and pushes a history entry, without
// reloading. The component stays mounted and its [ParamsHandler] runs, so
// this is the "patch" case: same view, different parameters.
func (b *Base) Navigate(ctx context.Context, rawURL string) error {
	return b.history(ctx, "pushState", rawURL, true)
}

// Replace changes the page's URL without adding a history entry, so the
// back button skips it. Right for a filter the user is still adjusting.
func (b *Base) Replace(ctx context.Context, rawURL string) error {
	return b.history(ctx, "replaceState", rawURL, true)
}

// Redirect leaves for another URL with a full page load, throwing this
// session away. Use it when the destination is not this component - a
// login page, another app.
func (b *Base) Redirect(_ context.Context, rawURL string) error {
	if b.n == nil {
		return ErrNotMounted
	}
	target, err := json.Marshal(rawURL)
	if err != nil {
		return err
	}
	return b.n.sess.enqueue(patch{
		script: fmt.Sprintf(`window.location.href = %s`, target),
	})
}

// history moves the address bar and, when notify is set, tells the
// component about its new parameters.
func (b *Base) history(ctx context.Context, method, rawURL string, notify bool) error {
	if b.n == nil {
		return ErrNotMounted
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("shuttle: navigating to %q: %w", rawURL, err)
	}
	sess := b.n.sess

	if err := sess.pushHistory(method, u.String()); err != nil {
		return err
	}
	if !notify {
		return nil
	}
	return sess.applyLocation(ctx, u)
}

// pushHistory moves the address bar without reloading.
func (s *Session) pushHistory(method, rawURL string) error {
	target, err := json.Marshal(rawURL)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.url = rawURL
	s.mu.Unlock()

	return s.enqueue(patch{
		script: fmt.Sprintf(`history.%s({}, '', %s)`, method, target),
	})
}

// applyLocation records where the page now is and tells every component
// that cares, which is what makes a URL change a live update rather than a
// remount.
//
// Every component, not just the root: a table reading its filter and page
// out of the URL is usually somebody's child.
func (s *Session) applyLocation(ctx context.Context, u *url.URL) error {
	p := Params(u.Query())

	s.mu.Lock()
	s.params = p
	if u.Path != "" {
		s.urlPath = u.Path
	}
	s.mu.Unlock()

	var failed error
	s.root.walk(func(n *node) {
		h, ok := n.cmp.(ParamsHandler)
		if !ok {
			return
		}
		if err := h.HandleParams(ctx, p); err != nil && failed == nil {
			failed = err
		}
	})
	if failed != nil {
		return failed
	}

	// The root's markup contains its children, so one push covers the tree.
	return s.root.push(ctx)
}

// syncURL writes the root component's URL state into the address bar, if it
// has any and it has changed.
//
// replaceState rather than pushState: a component re-rendering is not a
// navigation, and every keystroke in a filter box should not become a
// history entry the user has to press back through.
func (s *Session) syncURL() {
	// Every component that keeps state in the URL contributes, because the
	// one that owns a page's filters is usually a child rather than the
	// root. Two components using the same key would fight over it; that is
	// the app's to avoid, and the reason keys should read like the feature
	// they belong to.
	merged := url.Values{}
	found := false
	s.root.walk(func(n *node) {
		q, ok := n.cmp.(Queryer)
		if !ok {
			return
		}
		found = true
		maps.Copy(merged, q.QueryParams())
	})
	if !found {
		return
	}

	want := s.path(merged)

	s.mu.Lock()
	changed := want != s.url
	s.mu.Unlock()

	if !changed {
		return
	}
	if err := s.pushHistory("replaceState", want); err != nil && !errors.Is(err, ErrSessionClosed) {
		s.fail("sync url", err)
	}
}

// path renders the page's path with the given query.
func (s *Session) path(q url.Values) string {
	base := s.Path()
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// Back and forward are the half of navigation the server cannot see:
// Datastar has no router, and the attributes that would bind the query
// string are paid. So the page tells the server when history moves, and
// everything else - the re-render, the address bar - comes back down the
// stream it already has. That listener lives in the client shim; see
// shim.go.
