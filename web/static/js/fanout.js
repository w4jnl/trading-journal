// fanout.js - copy-trader fan-out rows in the journal trade tables
// (_journal_summary.html / groupLinkedTrades): one delegated click handler
// toggles a group's member rows. Was inline in the partial.
(function () {
  "use strict";
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('.fanout-btn');
    if (!b) return;
    var tr = b.closest('tr');
    var id = tr && tr.getAttribute('data-fanout-toggle');
    if (id == null) return;
    var open = b.getAttribute('aria-expanded') === 'true';
    b.setAttribute('aria-expanded', String(!open));
    tr.classList.toggle('open', !open);
    tr.parentNode.querySelectorAll('tr[data-fanout="' + id + '"]').forEach(function (r) { r.hidden = open; });
  });
})();
