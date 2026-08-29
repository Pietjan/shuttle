// The client shim is the only JavaScript shuttle ships. It is embedded into
// shim.go and emitted inside a module script, which is why it can declare a
// function at the top level and leave no global behind: the generated call
// at the bottom of that script is the only caller.
//
// nav is the navigation endpoint; up is the session-free health check. sid
// is this page's session and header is the name it travels under - the same
// header Datastar's own requests carry, because the id is a capability and
// a URL is the most copied string in a system.
function shuttleShim({ nav, up, sid, header }) {
  const root = document.documentElement;

  // Spelled out rather than built, so an app can grep for the class it
  // needs to style.
  const classes = {
    connecting: 'shuttle-connecting',
    connected: 'shuttle-connected',
    reconnecting: 'shuttle-reconnecting',
    dead: 'shuttle-dead',
  };

  let probe = null;
  const set = (state) => {
    if (root.dataset.shuttleState === state) return;
    root.dataset.shuttleState = state;
    for (const [s, cls] of Object.entries(classes)) {
      root.classList.toggle(cls, s === state);
    }
  };
  set('connecting');

  // Every fetch datastar makes dispatches this same event on document, so
  // the element that started one is the only way to tell the page's stream
  // from an action's request. The stream is opened from data-init; a click
  // is opened from the button.
  //
  // It matters for failures and not for successes. An action that fails -
  // a refused origin, a bad payload, a spent request budget - says nothing
  // about the connection, and reporting it as a lost one puts "connection
  // lost" over a stream that is working. A success is proof either way.
  const fromStream = (el) => el instanceof Element && el.matches('[data-init]');

  document.addEventListener('datastar-fetch', (e) => {
    const d = e.detail || {};

    if (String(d.type).startsWith('datastar-patch-')) {
      // A patch can only have come down the stream, so it is proof the
      // stream is alive - whatever request delivered it.
      set('connected');
    } else if (d.type === 'started') {
      // 'started' is only the request going out, so it proves nothing
      // about the stream unless it IS the stream: an action's fetch
      // starting must not paint a dead page 'connected' while the probe
      // below is still the only way back.
      if (fromStream(d.el)) set('connected');
    } else if (d.type === 'error' || d.type === 'retrying') {
      if (fromStream(d.el)) set('reconnecting');
    } else if (d.type === 'retries-failed' && fromStream(d.el)) {
      set('dead');
      // Datastar will not try again. Wait for the server to come back and
      // start over, rather than leaving a page that looks fine and is not.
      if (probe === null) {
        probe = setInterval(async () => {
          try {
            const r = await fetch(up, { cache: 'no-store' });
            if (r.ok) { clearInterval(probe); location.reload(); }
          } catch (_) { /* still down */ }
        }, 5000);
      }
    }
  });

  // A floating panel is open or it is not, and only the server knows
  // whether this render has anything to show - so it says so in
  // data-shuttle-open and this opens the popover to match.
  //
  // It runs after every patch because a re-render is exactly when the
  // answer changes. showPopover throws if the panel is already open, which
  // is the common case as someone types, so both calls are guarded by the
  // state we are moving away from.
  //
  // The value is applied only when it CHANGES, and that is the whole
  // light-dismiss story. Escape and an outside click close the panel in
  // the browser alone - the server still believes it is open, so any later
  // patch to the component (a parent's push, a reconnect) re-renders the
  // same open state, and re-applying it would pop a panel the user just
  // dismissed back open. The server bumps the value on every new search,
  // so typing reopens; everything else leaves the user's dismissal alone.
  const synced = new WeakMap();
  const syncPanels = () => {
    for (const owner of document.querySelectorAll('[data-shuttle-open]')) {
      const panel = owner.querySelector(':scope [popover]');
      if (!panel) continue;
      const value = owner.getAttribute('data-shuttle-open');
      if (synced.get(panel) === value) continue;
      const want = value !== 'false' && value !== '';
      const open = panel.matches(':popover-open');
      try {
        if (want && !open) panel.showPopover();
        else if (!want && open) panel.hidePopover();
        // Recorded only once applied, or a panel that was not yet in the
        // document would remember a state it never reached.
        synced.set(panel, value);
      } catch (_) { /* not connected yet; the next patch will do it */ }
    }
  };
  // Element patches only: a signal patch is the heartbeat, and syncing on
  // that would re-open a panel the user had dismissed, once every 25s.
  // An element patch means the component re-rendered, which is exactly when
  // the server's answer can have changed.
  document.addEventListener('datastar-fetch', (e) => {
    if (e.detail && e.detail.type === 'datastar-patch-elements') {
      // After the patch has been applied, not while it is being applied.
      requestAnimationFrame(syncPanels);
    }
  });

  // Roving focus. Arrow keys move focus between the rows of a container
  // marked data-shuttle-roving, which is what a filter-as-you-type list
  // needs and is deliberately all it needs: moving real focus keeps Enter
  // native and the rows honest about what they are, instead of painting
  // listbox roles onto links and buttons that then announce one thing and
  // do another.
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Escape') return;
    const container = e.target && e.target.closest && e.target.closest('[data-shuttle-roving]');
    if (!container) return;

    // Anything that uses arrow keys for its own editing keeps them. The
    // rove field is the one exception: ArrowDown out of it into the list
    // is the whole pattern.
    if (e.target instanceof Element &&
        e.target.matches('textarea, select, [contenteditable], input:not([data-shuttle-rove-field])')) {
      return;
    }

    const field = container.querySelector('[data-shuttle-rove-field]');
    if (e.key === 'Escape') {
      // preventDefault below also cancels the popover's own Esc handling,
      // so close it here rather than leaving it open under a focused field.
      const panel = container.querySelector(':scope [popover]');
      if (panel && panel.matches(':popover-open')) panel.hidePopover();
      if (field) { e.preventDefault(); field.focus(); }
      return;
    }

    const rows = Array.prototype.filter.call(
      container.querySelectorAll('[data-shuttle-rove-item]'),
      // checkVisibility where it exists: offsetParent is null for any
      // position:fixed element, visible or not.
      (el) => (el.checkVisibility ? el.checkVisibility() : el.offsetParent !== null) && !el.disabled,
    );
    if (rows.length === 0) return;
    e.preventDefault();

    const at = rows.indexOf(document.activeElement);
    const step = e.key === 'ArrowDown' ? 1 : -1;

    if (at < 0) {
      (step > 0 ? rows[0] : rows[rows.length - 1]).focus();
    } else if (at + step < 0) {
      if (field) field.focus();
    } else if (at + step >= rows.length) {
      rows[rows.length - 1].focus();
    } else {
      rows[at + step].focus();
    }
  });

  // Uploads. XMLHttpRequest rather than fetch, because fetch reports no
  // upload progress at all - that is the whole reason this exists.
  //
  // One transfer per input: picking again while one is in flight aborts
  // it, or two requests would fight over the same progress attributes and
  // whichever finished first would clear the state of the one still going.
  const uploading = new WeakMap();
  document.addEventListener('change', (e) => {
    const el = e.target;
    if (!(el instanceof HTMLInputElement)) return;
    const endpoint = el.getAttribute('data-shuttle-upload');
    if (!endpoint || !el.files || el.files.length === 0) return;

    const body = new FormData();
    for (const file of el.files) body.append('files', file, file.name);

    const prev = uploading.get(el);
    if (prev) prev.abort();

    const xhr = new XMLHttpRequest();
    uploading.set(el, xhr);

    const done = (state, detail) => {
      // An aborted transfer's callbacks fire after its replacement has
      // started; only the current transfer may touch the input's state.
      if (uploading.get(el) !== xhr) return;
      uploading.delete(el);
      el.removeAttribute('data-shuttle-uploading');
      el.removeAttribute('data-shuttle-progress');
      el.style.removeProperty('--shuttle-progress');
      if (state === 'error') {
        el.setAttribute('data-shuttle-upload-error', detail || 'upload failed');
      } else {
        el.removeAttribute('data-shuttle-upload-error');
      }
      // Cleared on failure too: change only fires when the selection
      // changes, so a value left in place would make retrying the same
      // file - the most likely thing to do after a failure - a no-op.
      el.value = '';
    };

    xhr.open('POST', endpoint, true);
    // After open, which is the only point a header can be set.
    xhr.setRequestHeader(header, sid);
    el.setAttribute('data-shuttle-uploading', '');
    el.removeAttribute('data-shuttle-upload-error');

    xhr.upload.addEventListener('progress', (p) => {
      if (!p.lengthComputable || uploading.get(el) !== xhr) return;
      const pct = Math.round((p.loaded / p.total) * 100);
      // The attribute is for selectors, the custom property for widths, so
      // a progress bar costs no round trips.
      el.setAttribute('data-shuttle-progress', String(pct));
      el.style.setProperty('--shuttle-progress', pct + '%');
    });
    xhr.addEventListener('load', () => {
      done(xhr.status >= 200 && xhr.status < 300 ? 'ok' : 'error', xhr.responseText);
    });
    xhr.addEventListener('error', () => done('error', 'upload failed'));
    xhr.addEventListener('abort', () => done('error', 'upload cancelled'));
    xhr.send(body);
  });

  window.addEventListener('popstate', async () => {
    try {
      const r = await fetch(nav, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', [header]: sid },
        body: JSON.stringify({ url: location.pathname + location.search }),
        keepalive: true,
      });
      // A refusal means the server cannot follow: the session is gone, or
      // history points somewhere this handler never wrote. The address bar
      // has already moved, so the only way the page and the URL agree
      // again is to load what the URL actually names.
      if (!r.ok) location.reload();
    } catch (_) { /* transport failures belong to the connection watcher */ }
  });
}
