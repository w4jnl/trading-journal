// Live refresh: pages that render the #live-refresh marker poll
// GET /data/version every 10s and reload when the server's data version
// differs from the one the page was rendered with. The compare is strict
// INEQUALITY, never "greater than": versions are process-local and restart
// at zero, so a post-deploy page must still refresh once.
//
// A reload is only automatic when the page is idle; mid-interaction (open
// trade sheet, row selection, open picker, focused field, in-flight htmx
// request) it shows a click-to-refresh toast instead and tries again on the
// next poll. URL query strings carry all filter/pagination state on these
// pages, so location.reload() never loses where you were.
(function () {
  'use strict';

  var marker = document.getElementById('live-refresh');
  if (!marker || !/^\d+$/.test(marker.dataset.version || '')) return;

  var renderedVersion = marker.dataset.version;
  var notifiedVersion = null;

  function busy() {
    if (document.querySelector('#trade-sheet-backdrop.show')) return true;
    if (document.querySelector('input.trade-sel:checked, #sel-all:checked')) return true;
    if (document.querySelector('details[open]')) return true;
    var el = document.activeElement;
    if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable)) return true;
    if (document.querySelector('.htmx-request')) return true;
    return false;
  }

  function notify(v) {
    if (notifiedVersion === v) return; // one toast per version
    notifiedVersion = v;
    if (typeof window.toast !== 'function') return;
    var t = window.toast('New data available — click to refresh', { kind: 'info', duration: 0 });
    if (t && t.addEventListener) {
      t.style.cursor = 'pointer';
      t.addEventListener('click', function () { location.reload(); });
    }
  }

  function check() {
    if (document.hidden) return;
    fetch('/data/version', { headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (!d || d.v == null) return;
        var v = String(d.v);
        if (v === renderedVersion) return;
        if (busy()) {
          // The compare stays against the RENDERED version, so even after a
          // dismissed toast the next idle poll still auto-reloads.
          notify(v);
          return;
        }
        location.reload();
      })
      .catch(function () { /* transient network trouble: next poll retries */ });
  }

  setInterval(check, 10000);
  // A hidden tab skips polls; catch up the moment it becomes visible again.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) check();
  });
})();
