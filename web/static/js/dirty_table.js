// dirty_table.js - shared bulk-edit dirty tracker for tabular editors
// (/accounts, /commissions, /admin/transactions). Extracted from the two
// near-verbatim page copies so the pattern can't drift.
//
// Conventions:
//   <form id="X" data-dirty-form>            owns the inputs (inline or via form="X")
//   <div class="save-bar" data-dirty-savebar="X">  sticky bar, shown while dirty;
//       inside it: [data-dirty-count]  changed-row counter
//                  [data-dirty-reset]  restores the loaded snapshot
//   Each changed input's closest <tr> gets .row-dirty (amber left edge, app.css).
//
// A form without a matching save bar is skipped (e.g. /commissions rendered
// read-only for non-admins). form.__dirtyReset() is exposed for page shims -
// the reconcile page's Cancel resets values before exiting edit mode.

(function () {
  "use strict";

  function getVal(el) {
    return el.type === "checkbox" ? (el.checked ? "1" : "0") : el.value;
  }
  function setVal(el, v) {
    if (el.type === "checkbox") el.checked = v === "1";
    else el.value = v;
  }

  function init(form) {
    var bar = document.querySelector('[data-dirty-savebar="' + form.id + '"]');
    if (!bar) return;
    var count = bar.querySelector("[data-dirty-count]");
    var resetBtn = bar.querySelector("[data-dirty-reset]");
    var inputs = Array.prototype.filter.call(form.elements, function (el) {
      return el.tagName === "INPUT" || el.tagName === "SELECT";
    });
    var snapshot = new Map();
    inputs.forEach(function (el) { snapshot.set(el, getVal(el)); });

    function refresh() {
      var dirtyRows = new Set();
      inputs.forEach(function (el) {
        if (getVal(el) !== snapshot.get(el)) {
          var tr = el.closest("tr");
          if (tr) dirtyRows.add(tr);
        }
      });
      document.querySelectorAll("tr.row-dirty").forEach(function (tr) {
        if (!dirtyRows.has(tr)) tr.classList.remove("row-dirty");
      });
      dirtyRows.forEach(function (tr) { tr.classList.add("row-dirty"); });
      bar.classList.toggle("dirty", dirtyRows.size > 0);
      if (count) count.textContent = dirtyRows.size;
    }

    inputs.forEach(function (el) {
      el.addEventListener("input", refresh);
      el.addEventListener("change", refresh);
    });

    function doReset() {
      inputs.forEach(function (el) { setVal(el, snapshot.get(el)); });
      refresh();
    }
    if (resetBtn) resetBtn.addEventListener("click", doReset);
    form.__dirtyReset = doReset;

    // Cmd-S / Ctrl-S submits the bulk save while the dirty bar is visible.
    // Suppressed in multi-line contexts (defensive - no textarea here today).
    document.addEventListener("keydown", function (e) {
      if (!(e.metaKey || e.ctrlKey) || (e.key !== "s" && e.key !== "S")) return;
      if (!bar.classList.contains("dirty")) return;
      if (e.target && e.target.tagName === "TEXTAREA") return;
      e.preventDefault();
      if (form.requestSubmit) form.requestSubmit();
      else form.submit();
    });
  }

  document.querySelectorAll("form[data-dirty-form]").forEach(init);
})();
