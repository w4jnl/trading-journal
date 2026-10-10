// date_presets.js - the from/to date presets (_date_presets.html): a
// [data-preset] button fills the owning form's from/to inputs and submits.
(function () {
  "use strict";
  var fmt = function (d) { return d.toISOString().slice(0, 10); };
  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('[data-preset]');
    if (!btn) return;
    var form = btn.closest('form');
    if (!form) return;
    var fromInput = form.querySelector('input[name="from"]');
    var toInput = form.querySelector('input[name="to"]');
    if (!fromInput || !toInput) return;
    var today = new Date();
    var from = new Date(today);
    if (btn.dataset.preset === 'thisweek') {
      // Monday of the current ISO-style week. JS getDay(): 0=Sun..6=Sat.
      var daysSinceMon = (today.getDay() + 6) % 7;
      from.setDate(today.getDate() - daysSinceMon);
    }
    if (btn.dataset.preset === 'week') from.setDate(today.getDate() - 7);
    if (btn.dataset.preset === 'month') from.setDate(today.getDate() - 30);
    fromInput.value = fmt(from);
    toInput.value = fmt(today);
    form.submit();
  });
})();
