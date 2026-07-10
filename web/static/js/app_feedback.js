// app_feedback.js - global HTMX feedback plumbing, loaded on every page.
//
// 1. HX-Trigger "toast" bridge: a handler sets the response header
//      HX-Trigger: {"toast": {"message": "...", "kind": "success"}}
//    and htmx fires a `toast` CustomEvent on document.body with detail set to
//    the JSON value; we forward it to window.toast (toast.js). This bridge
//    used to live only on /accounts, so HX-Trigger toasts from any other page
//    were silently dropped.
//
// 2. Error surfacing: htmx does not swap non-2xx responses, so without these
//    listeners a failed hx-* request (AI analyze 502/409, journal-save-during-
//    AI 409, bulk saves) just stopped its spinner and told the user nothing.
//    toast.js already renders kind:"error" with role="alert".

(function () {
  "use strict";

  function notify(msg, kind) {
    if (typeof window.toast === "function") {
      window.toast(msg, { kind: kind || "info" });
    }
  }

  // Deferred scripts run after DOM parse, so document.body exists here.
  document.body.addEventListener("toast", function (e) {
    if (!e.detail || !e.detail.message) return;
    notify(e.detail.message, e.detail.kind);
  });

  // Server answered an HTMX request with a non-2xx status.
  document.body.addEventListener("htmx:responseError", function (e) {
    var xhr = e.detail && e.detail.xhr;
    var status = xhr ? xhr.status : 0;
    var body = ((xhr && xhr.responseText) || "").trim();
    // http.Error bodies are short plain text; anything long or HTML-shaped
    // gets a generic message instead of a wall of markup.
    if (!body || body.length > 200 || body.charAt(0) === "<") {
      body = "request failed";
    }
    notify("Error " + status + ": " + body, "error");
  });

  // Network-level failure (server down, connection dropped mid-request).
  document.body.addEventListener("htmx:sendError", function () {
    notify("Network error - the server did not respond.", "error");
  });
})();
