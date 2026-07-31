// The client shim is the only JavaScript shuttle ships. It is embedded into
// shim.go and emitted inside a module script, which is why it can declare a
// function at the top level and leave no global behind: the generated call
// at the bottom of that script is the only caller.
//
// nav is this session's navigation endpoint; up is the session-free health
// check.
function shuttleShim({ nav, up }) {
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

  document.addEventListener('datastar-fetch', (e) => {
    const d = e.detail || {};

    if (d.type === 'started' || String(d.type).startsWith('datastar-patch-')) {
      // A patch can only have come down the stream, so it is proof the
      // stream is alive. 'started' is here too, so a freshly loaded page
      // does not sit on 'connecting' until the first heartbeat.
      set('connected');
    } else if (d.type === 'error' || d.type === 'retrying') {
      set('reconnecting');
    } else if (d.type === 'retries-failed') {
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
  const syncPanels = () => {
    for (const root of document.querySelectorAll('[data-shuttle-open]')) {
      const panel = root.querySelector(':scope [popover]');
      if (!panel) continue;
      const want = root.getAttribute('data-shuttle-open') === 'true';
      const open = panel.matches(':popover-open');
      try {
        if (want && !open) panel.showPopover();
        else if (!want && open) panel.hidePopover();
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
      (el) => el.offsetParent !== null && !el.disabled,
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
  document.addEventListener('change', (e) => {
    const el = e.target;
    if (!(el instanceof HTMLInputElement)) return;
    const endpoint = el.getAttribute('data-shuttle-upload');
    if (!endpoint || !el.files || el.files.length === 0) return;

    const body = new FormData();
    for (const file of el.files) body.append('files', file, file.name);

    const done = (state, detail) => {
      el.removeAttribute('data-shuttle-uploading');
      el.removeAttribute('data-shuttle-progress');
      el.style.removeProperty('--shuttle-progress');
      if (state === 'error') {
        el.setAttribute('data-shuttle-upload-error', detail || 'upload failed');
      } else {
        el.removeAttribute('data-shuttle-upload-error');
        el.value = '';
      }
    };

    const xhr = new XMLHttpRequest();
    xhr.open('POST', endpoint, true);
    el.setAttribute('data-shuttle-uploading', '');
    el.removeAttribute('data-shuttle-upload-error');

    xhr.upload.addEventListener('progress', (p) => {
      if (!p.lengthComputable) return;
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

  window.addEventListener('popstate', () => {
    fetch(nav, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: location.pathname + location.search }),
      keepalive: true,
    });
  });
}
