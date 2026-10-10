// back_button.js - Esc navigates to the page's back link (_back_button.html
// renders <a class="back-button" href=...> when the page was reached via a
// cross-page deep link). Mirrors the trade sheet's close-on-Esc.
(function () {
  "use strict";
  var a = document.querySelector('a.back-button[href]');
  var back = a && a.getAttribute('href');
  if (!back) return;
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    // The trade sheet and the command palette own their own Esc.
    var sheet = document.getElementById('trade-sheet-backdrop');
    if (sheet && sheet.classList.contains('show')) return;
    var cmdk = document.getElementById('cmdk-backdrop');
    if (cmdk && cmdk.classList.contains('show')) return;
    // Never navigate over unsaved journal text, and never steal Esc from a
    // form field (its own Esc semantics: close pickers, cancel edits).
    if (window.journalEditorDirty && window.journalEditorDirty()) return;
    var el = document.activeElement;
    if (el && (el.matches('input, textarea, select') || el.isContentEditable)) return;
    e.preventDefault();
    window.location.href = back;
  });
})();
