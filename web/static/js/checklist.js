// Playbook checklist on the entry editors: every strategy's checklist is
// rendered as a hidden block; the one matching the strategy <select> of the
// same form shows. Delegated, so htmx-swapped editors (the trade sheet)
// work without re-binding.
(function () {
  "use strict";
  document.addEventListener("change", function (e) {
    var sel = e.target;
    if (!sel || sel.name !== "strategy_id" || !sel.form) return;
    var picker = sel.form.querySelector("[data-checklist-picker]");
    if (!picker) return;
    var want = String(sel.value || "0");
    picker.querySelectorAll("[data-checklist-for]").forEach(function (block) {
      block.hidden = block.getAttribute("data-checklist-for") !== want;
    });
  });
})();
