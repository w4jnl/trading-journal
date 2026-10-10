// account_filter.js - the account picker (_account_filter.html): live
// summary count as checkboxes toggle, click-outside closes the dropdown
// like a native select. The picker sits outside the live-refresh regions,
// so binding once at load is enough.
(function () {
  "use strict";
  var pickers = document.querySelectorAll('.filter-picker');
  if (!pickers.length) return;
  pickers.forEach(function (p) {
    var countEl = p.querySelector('.filter-count');
    if (!countEl) return;
    var emptyLabel = countEl.dataset.emptyLabel || 'All accounts';
    p.addEventListener('change', function () {
      var n = p.querySelectorAll('input[type=checkbox]:checked').length;
      countEl.textContent = n === 0 ? emptyLabel : n === 1 ? '1 selected' : n + ' selected';
    });
  });
  document.addEventListener('click', function (e) {
    pickers.forEach(function (p) {
      if (p.open && !p.contains(e.target)) p.open = false;
    });
  });
})();
