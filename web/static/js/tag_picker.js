// tag_picker.js - the journal tag picker (good / neutral / bad / none).
// Toggles .chip-on on the chosen label and clears it from its siblings, so
// the selected look never depends on :has() support. Delegated on document:
// pickers arrive inside HTMX swaps (trade sheet, journal pages) too.
(function () {
  "use strict";
  document.addEventListener("change", function (e) {
    var input = e.target;
    if (!input || input.type !== "radio" || input.name !== "tag") return;
    var picker = input.closest(".tag-picker");
    if (!picker) return;
    picker.querySelectorAll(".chip").forEach(function (c) { c.classList.remove("chip-on"); });
    var label = input.closest("label.chip");
    if (label) label.classList.add("chip-on");
  });
})();
