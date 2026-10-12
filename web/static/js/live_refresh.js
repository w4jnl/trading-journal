// Live refresh: pages that render the #live-refresh marker poll
// GET /data/version every 10s and, when the server's data version differs
// from the marker's, fetch the SAME URL with fragment=1. The server answers
// with an out-of-band htmx bundle - each dynamic region re-rendered with
// hx-swap-oob plus a refreshed marker - so only the data regions repaint:
// no full reload, no flash, no scroll jump, and static chrome (nav, filter
// forms, style blocks) is never touched.
//
// The version compare is strict INEQUALITY, never "greater than": versions
// are process-local and restart at zero, so a post-deploy page must still
// refresh once. The marker is re-read from the DOM on every check because
// the update swaps it.
//
// Applying: the bundle is fetched first and compared with the page. Rows
// (tr[data-trade-id]) that merely changed are swapped in place and flash
// briefly, even while a trade sheet is open; rows added, removed or
// reordered while the page is busy (open sheet, row selection, open picker)
// defer to a click-to-refresh toast, since the list shifting under an open
// sheet is disorienting. A region that holds the trader's input right now
// (focus, unsaved edits, an in-flight request) is never swapped under them.
(function () {
  'use strict';

  if (!document.getElementById('live-refresh')) return;

  var TOAST_TEXT = 'New data available';

  function version() {
    var m = document.getElementById('live-refresh');
    return m && /^\d+$/.test(m.dataset.version || '') ? m.dataset.version : null;
  }

  function fragmentURL() {
    return location.pathname + location.search + (location.search ? '&' : '?') + 'fragment=1';
  }

  // busy: the page is mid-interaction, so rows must not come and go under
  // the trader. The trades filter panel is a <details> that ships open on
  // desktop and is not an interaction; pickers and disclosures are.
  function busy() {
    if (document.querySelector('#trade-sheet-backdrop.show')) return true;
    if (document.querySelector('input.trade-sel:checked, #sel-all:checked')) return true;
    if (document.querySelector('details[open]:not(.filters-collapse)')) return true;
    return false;
  }

  // modified reports a field whose value differs from its server-rendered
  // default: an unsaved edit, focused or not.
  function modified(root) {
    var fields = root.querySelectorAll('textarea, input[type="text"], input[type="number"], input[type="date"]');
    for (var i = 0; i < fields.length; i++) {
      if (fields[i].value !== fields[i].defaultValue) return true;
    }
    return false;
  }

  // holdsInput: swapping this region would take something from under the
  // trader's hands.
  function holdsInput(region) {
    var el = document.activeElement;
    if (el && el !== document.body && region.contains(el)) return true;
    // A form the user unfolded (rendered closed, open now) would fold back
    // under them; one rendered open comes back open.
    if (region.querySelector('details[data-keep-open][open]:not([data-rendered-open])')) return true;
    if (region.querySelector('.row-dirty, .htmx-request')) return true;
    return modified(region);
  }

  function rows(root) {
    return root ? Array.prototype.slice.call(root.querySelectorAll('tr[data-trade-id]')) : [];
  }

  // signature is a row's markup with what the browser changes after render
  // taken out: <time> elements are localised in place (text and a marker
  // attribute), htmx stamps transient classes, and the flash is ours.
  function signature(tr) {
    var c = tr.cloneNode(true);
    var times = c.querySelectorAll('time[datetime]');
    for (var i = 0; i < times.length; i++) {
      times[i].parentNode.replaceChild(document.createTextNode(times[i].getAttribute('datetime')), times[i]);
    }
    var all = c.querySelectorAll('[class]');
    for (var j = 0; j < all.length; j++) {
      all[j].classList.remove('htmx-request', 'htmx-added', 'htmx-settling', 'htmx-swapping', 'row-flash');
      if (!all[j].classList.length) all[j].removeAttribute('class');
    }
    c.classList.remove('htmx-request', 'htmx-added', 'htmx-settling', 'htmx-swapping', 'row-flash');
    return c.outerHTML.replace(/\s+/g, ' ').trim();
  }

  // diff compares the fetched bundle with the page. structural: rows were
  // added, removed or reordered in some region (or a region is new).
  // changed: ids of rows whose markup differs. blocked: a region to be
  // swapped holds the trader's input.
  function diff(doc) {
    var out = { structural: false, changed: [], blocked: false };
    var regions = doc.querySelectorAll('[hx-swap-oob]');
    for (var i = 0; i < regions.length; i++) {
      var next = regions[i];
      if (!next.id || next.id === 'live-refresh') continue;
      var cur = document.getElementById(next.id);
      if (!cur) { out.structural = true; continue; }
      if (holdsInput(cur)) out.blocked = true;
      var a = rows(next), b = rows(cur);
      if (!a.length && !b.length) continue;
      var key = function (tr) { return tr.getAttribute('data-trade-id'); };
      var ida = a.map(key), idb = b.map(key);
      if (ida.join('|') !== idb.join('|')) { out.structural = true; continue; }
      for (var j = 0; j < a.length; j++) {
        if (signature(a[j]) !== signature(b[j])) out.changed.push(ida[j]);
      }
    }
    return out;
  }

  // swapBundle lands the fetched bundle (every element is hx-swap-oob, the
  // marker included), keeps the row selection, and flashes changed rows.
  function swapBundle(html, changed) {
    var checked = Array.prototype.map.call(document.querySelectorAll('input.trade-sel:checked'), function (cb) { return cb.value; });
    htmx.swap(document.body, html, { swapStyle: 'none' });
    checked.forEach(function (v) {
      var cb = document.querySelector('input.trade-sel[value="' + v + '"]');
      if (cb && !cb.checked) {
        cb.checked = true;
        cb.dispatchEvent(new Event('change', { bubbles: true }));
      }
    });
    changed.forEach(function (id) {
      var tr = document.querySelector('tr[data-trade-id="' + id + '"]');
      if (!tr) return;
      tr.classList.add('row-flash');
      setTimeout(function () { tr.classList.remove('row-flash'); }, 1800);
    });
  }

  // reload is the one action that always works: the toast's Refresh, and
  // the fallback when htmx is missing or a swap throws.
  function reload() {
    if (!window.htmx) { location.reload(); return; }
    try {
      htmx.ajax('GET', fragmentURL(), { target: 'body', swap: 'none' }).catch(function () {});
    } catch (e) {
      location.reload();
    }
  }

  function notify() {
    if (typeof window.toast !== 'function') return;
    // One toast at a time, whatever the version: a second one would only
    // repeat the first.
    var texts = document.querySelectorAll('#toast-stack .toast:not(.toast--leaving) .toast-text');
    for (var i = 0; i < texts.length; i++) {
      if (texts[i].textContent === TOAST_TEXT) return;
    }
    window.toast(TOAST_TEXT, { kind: 'info', duration: 0, action: { label: 'Refresh', onClick: reload } });
  }

  function update(v) {
    if (!window.htmx) { location.reload(); return; }
    fetch(fragmentURL(), { headers: { Accept: 'text/html' } })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(new Error('fragment ' + r.status)); })
      .then(function (html) {
        if (v !== null && version() !== null && v === version()) return; // landed meanwhile
        var doc = new DOMParser().parseFromString(html, 'text/html');
        var d = diff(doc);
        if (d.blocked || (d.structural && busy())) {
          if (window.console && console.debug) console.debug('live-refresh: update to v' + v + ' deferred (' + (d.blocked ? 'region in use' : 'rows changed while busy') + ') - toast shown');
          notify();
          return;
        }
        if (window.console && console.debug) console.debug('live-refresh: applying update to v' + v + (d.changed.length ? ', ' + d.changed.length + ' row(s) changed' : ''));
        try {
          swapBundle(html, d.changed);
        } catch (e) {
          reload();
        }
      })
      .catch(function (e) {
        // Transient network trouble retries on the next poll; a programming
        // error must not hide behind the same silence.
        if (window.console && console.debug) console.debug('live-refresh: update failed', e);
      });
  }

  function check() {
    if (document.hidden) return;
    var mine = version();
    if (mine == null) return;
    fetch('/data/version', { headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (!d || d.v == null) return;
        var v = String(d.v);
        if (v === version()) return; // re-read: an update may just have landed
        update(v);
      })
      .catch(function () { /* transient network trouble: next poll retries */ });
  }

  setInterval(check, 10000);
  // A hidden tab skips polls; catch up the moment it becomes visible again.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) check();
  });
})();
