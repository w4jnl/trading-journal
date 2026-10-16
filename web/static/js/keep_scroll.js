// keep_scroll.js - a page that saves through plain forms (POST, redirect,
// GET) reloads at the top, so a change far down the page (Settings: the
// guardrail switch, pairing a phone) puts the trader back at the heading
// and they scroll down again after every click. A container opts in with
// data-keep-scroll="<key>": a form inside it that really navigates
// (htmx-driven and cancelled submits do not) saves the scroll position
// under the key, and the next page carrying the same key restores it
// once, within half a minute. The flash of the save is already a toast
// (app_feedback.js), so nothing at the top goes unseen; a result that must
// be seen and sits elsewhere (a new API key, shown once) is marked
// data-keep-scroll-show and the page scrolls to it instead.
(function () {
  "use strict";

  var store = "tj.keepScroll";
  var maxAge = 30 * 1000;

  function read() {
    try { return JSON.parse(sessionStorage.getItem(store) || "null"); } catch (e) { return null; }
  }
  function write(v) {
    try {
      if (v) sessionStorage.setItem(store, JSON.stringify(v)); else sessionStorage.removeItem(store);
    } catch (e) { /* private mode: the page just opens at the top */ }
  }

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement) || e.defaultPrevented) return;
    var root = form.closest("[data-keep-scroll]");
    if (!root || form.hasAttribute("hx-post") || form.hasAttribute("hx-get") || form.target) return;
    write({ key: root.getAttribute("data-keep-scroll"), y: window.scrollY, t: Date.now() });
  });

  var saved = read();
  if (!saved) return;
  write(null);
  var here = document.querySelector("[data-keep-scroll]");
  if (!here || here.getAttribute("data-keep-scroll") !== saved.key || Date.now() - saved.t > maxAge) return;
  // Where the trader was wins over a card anchor in the address (the
  // redirect's "#ai"): the anchor puts the card at the top, the saved
  // position puts the click back under the finger. The browser may scroll
  // to the anchor again at load, so restore once more then.
  var show = document.querySelector("[data-keep-scroll-show]");
  var restore = function () {
    if (show) show.scrollIntoView({ block: "start" }); else window.scrollTo(0, saved.y);
  };
  restore();
  window.addEventListener("load", restore, { once: true });
})();
