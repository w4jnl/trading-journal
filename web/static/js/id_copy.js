/* Click-to-copy for [data-copy-id] chips (the .id-ref trade/journal IDs).

   Copies the bare ID to the clipboard and toasts which entity it was, so the
   user can quickly reference it to the AI ("analyze trade 4412"). One delegated
   listener so it also works for HTMX-swapped content (the trade detail sheet).
   Falls back to execCommand for non-secure-context (plain-HTTP) deployments
   where navigator.clipboard is unavailable. */
(function () {
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      try {
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.setAttribute('readonly', '');
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        var ok = document.execCommand('copy');
        document.body.removeChild(ta);
        if (ok) resolve(); else reject(new Error('execCommand failed'));
      } catch (err) {
        reject(err);
      }
    });
  }

  document.addEventListener('click', function (e) {
    var el = e.target.closest('[data-copy-id]');
    if (!el) return;
    var id = el.getAttribute('data-copy-id');
    if (!id) return;
    var kind = el.getAttribute('data-copy-kind') || 'ID';
    copyText(id).then(function () {
      if (window.toast) window.toast('Copied ' + kind + ' ID ' + id, { kind: 'success', duration: 1500 });
    }).catch(function () {
      if (window.toast) window.toast('Copy failed - select ' + kind + ' ID ' + id + ' manually', { kind: 'error' });
    });
  });
})();
