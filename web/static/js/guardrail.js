// The intraday guardrail's page side (internal/guardrail records the
// nudges). While the trader has it on (<meta name="tj-guardrail">), every
// 15 s take the new nudges and show each once: a toast that stays until
// dismissed and, while the tab is in the background and notifications are
// allowed, a browser notification. A background tab without notification
// rights leaves them for a visible one. Also wires the Settings button that
// asks this browser for the notification permission.
(function () {
  "use strict";

  var EVERY = 15000;

  function csrf() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.getAttribute("content") || "" : "";
  }

  function canNotify() {
    return "Notification" in window && Notification.permission === "granted";
  }

  function show(n) {
    var text = n.title + " — " + n.detail + (n.lesson ? " Your lesson: " + n.lesson : "");
    if (window.toast) {
      window.toast(text, {
        kind: n.severity === "warn" ? "warn" : "info",
        duration: 0,
        action: n.href ? { label: "Open the day", href: n.href } : undefined,
      });
    }
    if (document.hidden && canNotify()) {
      try {
        new Notification(n.title, { body: n.detail + (n.lesson ? "\nYour lesson: " + n.lesson : ""), tag: "tj-nudge-" + n.id });
      } catch (e) { /* some browsers only notify from a service worker */ }
    }
  }

  var busy = false;
  function take() {
    if (busy || (document.hidden && !canNotify())) return;
    busy = true;
    fetch("/nudges/take", {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": csrf(), "Accept": "application/json" },
    })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.nudges) d.nudges.forEach(show); })
      .catch(function () { /* offline or restarting: the next poll retries */ })
      .then(function () { busy = false; });
  }

  function bindPermission() {
    document.querySelectorAll("[data-notify-permission]").forEach(function (btn) {
      var out = document.querySelector(btn.getAttribute("data-notify-permission"));
      function state() {
        if (!out) return;
        if (!("Notification" in window)) out.textContent = "This browser has no notifications.";
        else if (Notification.permission === "granted") out.textContent = "Allowed in this browser.";
        else if (Notification.permission === "denied") out.textContent = "Blocked in this browser: allow them in the site settings.";
        else out.textContent = "Not asked yet in this browser.";
      }
      state();
      btn.addEventListener("click", function () {
        if ("Notification" in window) Notification.requestPermission().then(state);
      });
    });
  }

  function start() {
    bindPermission();
    if (!document.querySelector('meta[name="tj-guardrail"]')) return;
    take();
    setInterval(take, EVERY);
    document.addEventListener("visibilitychange", function () { if (!document.hidden) take(); });
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
