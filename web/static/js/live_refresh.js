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
// Updates only apply when the page is idle; mid-interaction (open trade
// sheet, row selection, open picker, focused field, unsaved edits, in-flight
// htmx request) they defer to a click-to-refresh toast, and the next idle
// poll applies automatically.
(function () {
  'use strict';

  if (!document.getElementById('live-refresh')) return;

  var toastShownFor = null;

  function version() {
    var m = document.getElementById('live-refresh');
    return m && /^\d+$/.test(m.dataset.version || '') ? m.dataset.version : null;
  }

  function busy() {
    if (document.querySelector('#trade-sheet-backdrop.show')) return true;
    if (document.querySelector('input.trade-sel:checked, #sel-all:checked')) return true;
    if (document.querySelector('details[open]')) return true;
    var el = document.activeElement;
    if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable)) return true;
    if (document.querySelector('.htmx-request')) return true;
    // Unsaved edits, even unfocused: bulk-edit tables mark rows dirty
    // (dirty_table.js), and a typed-but-unfocused field differs from its
    // server-rendered default (journal notes, filter inputs).
    if (document.querySelector('.row-dirty')) return true;
    var fields = document.querySelectorAll('textarea, input[type="text"], input[type="number"], input[type="date"]');
    for (var i = 0; i < fields.length; i++) {
      if (fields[i].value !== fields[i].defaultValue) return true;
    }
    return false;
  }

  function apply() {
    if (!window.htmx) { location.reload(); return; }
    var url = location.pathname + location.search + (location.search ? '&' : '?') + 'fragment=1';
    // swap:'none' - everything in the response is hx-swap-oob and lands by
    // element id, including the refreshed #live-refresh marker. The explicit
    // target is required: htmx.ajax without one throws synchronously (2.0.3)
    // even though swap:'none' never uses it.
    try {
      htmx.ajax('GET', url, { target: 'body', swap: 'none' }).catch(function () {
        /* transient trouble: marker unchanged, so the next poll retries */
      });
    } catch (e) {
      // A synchronous throw is a programming/environment error, not transient
      // network trouble - fall back to the one action that always works.
      location.reload();
    }
  }

  function notify(v) {
    if (toastShownFor === v) return; // one toast per version
    toastShownFor = v;
    if (typeof window.toast !== 'function') return;
    window.toast('New data available', { kind: 'info', duration: 0, action: { label: 'Refresh', onClick: apply } });
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
        if (busy()) {
          if (window.console && console.debug) console.debug('live-refresh: update to v' + v + ' deferred (page busy) - toast shown');
          notify(v);
          return;
        }
        if (window.console && console.debug) console.debug('live-refresh: applying update to v' + v);
        apply();
      })
      .catch(function () { /* transient network trouble: next poll retries */ });
  }

  setInterval(check, 10000);
  // A hidden tab skips polls; catch up the moment it becomes visible again.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) check();
  });
})();
