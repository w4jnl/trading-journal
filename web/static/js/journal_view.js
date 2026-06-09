/* Journal entry view<->edit toggle.

   A saved entry (non-empty body) is rendered server-side as read-only markdown
   inside [data-journal-entry], next to its edit <form class="journal-form">,
   which the server marks hidden. Clicking "Edit" reveals the form (and focuses
   the editor); "Cancel" restores the read-only view. A new/empty entry has no
   view panel - the form is shown directly, so the page opens in edit mode.

   One delegated listener covers the page (there is only ever one entry section
   per journal page). Mirrors the Library row behavior in library.html. */
(function () {
  document.addEventListener('click', function (e) {
    var el = e.target.closest('[data-journal-edit], [data-journal-cancel]');
    if (!el) return;
    var section = el.closest('[data-journal-entry]');
    if (!section) return;
    var view = section.querySelector('[data-journal-view]');
    var form = section.querySelector('.journal-form');
    if (el.hasAttribute('data-journal-edit')) {
      if (view) view.hidden = true;
      if (form) {
        form.hidden = false;
        var ta = form.querySelector('textarea[data-je-input]');
        if (ta) ta.focus();
      }
    } else {
      if (form) form.hidden = true;
      if (view) view.hidden = false;
    }
  });
})();
