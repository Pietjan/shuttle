package shuttle

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// The client shim is the only JavaScript shuttle ships, and it exists for
// the two things the server genuinely cannot see for itself: the back
// button, and the state of its own connection.
//
// It lives in shim.js rather than in a Go string so that it is JavaScript as
// far as an editor, a formatter and a linter are concerned, and so that
// nothing in it has to be escaped for Go's sake. The only part that varies
// per session is one generated call appended at the bottom of the script.
//
// Connection state comes from Datastar's `datastar-fetch` event. Three
// things about it were verified in a browser rather than assumed, and each
// one would otherwise have made the watcher silently do nothing:
//
//   - it is dispatched on `document` with bubbles: false, so a listener on
//     window never sees it;
//   - detail.url does not exist, so the page's own stream cannot be told
//     apart from an action's fetch that way;
//   - detail.type carries the SSE event names too (datastar-patch-elements,
//     datastar-patch-signals), not only the fetch lifecycle.
//
// That last one is what makes this work: a patch event can only have come
// down the stream, so it is proof the stream is alive - and the server's
// heartbeat sends one on a timer, so the proof keeps arriving on an idle
// page.
//
// `retries-failed` matters most: after retryMaxCount attempts Datastar
// gives up permanently, and without this the page would simply go quiet
// with nothing to show for it.
//
//go:embed shim.js
var shimSource string

// clientScript renders the shim for one session. nonce, when set, is the
// page's CSP nonce - already validated by the handler, and required on this
// tag for any script-src without 'unsafe-inline'.
//
// type="module" is not decoration: it is what keeps shuttleShim out of the
// global scope, so a page carrying this script gains no names of its own.
func clientScript(s *Session, nonce string) string {
	base := s.prefix + routePrefix
	nav, err := json.Marshal(base + "/nav")
	if err != nil {
		return ""
	}
	// The shim makes two requests Datastar does not: the popstate report and
	// the upload. Both carry the session the same way every other one does,
	// so it needs the id rather than a URL with the id in it.
	sid, err := json.Marshal(s.id)
	if err != nil {
		return ""
	}
	header, err := json.Marshal(SessionHeader)
	if err != nil {
		return ""
	}
	// The health check is deliberately session-free: a page polling it after
	// its own session died must not mint a new one on every attempt.
	up, err := json.Marshal(base + "/up")
	if err != nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("<script type=\"module\"")
	if nonce != "" {
		b.WriteString(" nonce=\"")
		b.WriteString(nonce)
		b.WriteString("\"")
	}
	b.WriteString(">\n")
	b.WriteString(shimSource)
	b.WriteString("\nshuttleShim({nav: ")
	b.Write(nav)
	b.WriteString(", up: ")
	b.Write(up)
	b.WriteString(", sid: ")
	b.Write(sid)
	b.WriteString(", header: ")
	b.Write(header)
	b.WriteString("});\n</script>\n")
	return b.String()
}
