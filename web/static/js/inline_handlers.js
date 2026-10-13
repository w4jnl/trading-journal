// inline_handlers.js - the behaviours templates used to attach with inline
// on* attributes, as data attributes, so the Content Security Policy can
// drop 'unsafe-inline' for scripts.
//
//   data-autosubmit            change -> submit the owning form
//   data-goto="/p/" [data-goto-suffix="?x"]
//                              change -> navigate to prefix + value + suffix (empty value: nothing)
//   data-reload                click  -> reload the page
//   data-confirm="Question?"   submit (forms) / click (buttons, links) -> confirm() or cancel
//   data-busy="Label"          submit -> disable the form's button and relabel it
//   data-stop                  click  -> stop propagation (a cell inside a clickable row)
//   data-prevent               click  -> preventDefault (an anchor whose click htmx handles)
//   data-set-field="name" data-set-value="v"
//                              click  -> set the owning form's field to v (fires change)
//
// data-stop and data-prevent must act before the row's or htmx's own
// listeners, so they are bound on the elements themselves (at load and
// after every htmx swap); the rest is delegated on document.
(function () {
  "use strict";

  document.addEventListener("change", function (e) {
    var el = e.target;
    if (!(el instanceof Element)) return;
    if (el.hasAttribute("data-autosubmit")) {
      if (el.form) el.form.requestSubmit();
      return;
    }
    var prefix = el.getAttribute("data-goto");
    if (prefix != null && el.value) {
      window.location.href = prefix + el.value + (el.getAttribute("data-goto-suffix") || "");
    }
  });

  document.addEventListener("click", function (e) {
    var t = e.target instanceof Element ? e.target : null;
    if (!t) return;
    var ask = t.closest("button[data-confirm], a[data-confirm]");
    if (ask && !window.confirm(ask.getAttribute("data-confirm"))) {
      e.preventDefault();
      e.stopImmediatePropagation();
      return;
    }
    if (t.closest("[data-reload]")) window.location.reload();
    var set = t.closest("button[data-set-field]");
    var field = set && set.form && set.form.elements.namedItem(set.getAttribute("data-set-field"));
    if (field && "value" in field) {
      field.value = set.getAttribute("data-set-value") || "";
      field.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    var q = form.getAttribute("data-confirm");
    if (q != null && !window.confirm(q)) {
      e.preventDefault();
      e.stopImmediatePropagation();
      return;
    }
    var busy = form.getAttribute("data-busy");
    if (busy != null) {
      var b = form.querySelector("button");
      if (b) { b.disabled = true; b.textContent = busy; }
    }
  });

  function bind(root) {
    root.querySelectorAll("[data-stop]").forEach(function (el) {
      if (el.__stopBound) return;
      el.__stopBound = true;
      el.addEventListener("click", function (e) { e.stopPropagation(); });
    });
    root.querySelectorAll("[data-prevent]").forEach(function (el) {
      if (el.__preventBound) return;
      el.__preventBound = true;
      el.addEventListener("click", function (e) { e.preventDefault(); });
    });
  }
  bind(document);
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (e.target instanceof Element) bind(e.target);
  });
  document.body.addEventListener("htmx:oobAfterSwap", function (e) {
    if (e.target instanceof Element) bind(e.target);
  });
})();
