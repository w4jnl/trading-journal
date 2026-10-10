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
//
// 3. Flash bridge: pages still render server-side .flash banners after a
//    redirect (?flash=saved, ?err=..., ?mwfills=N). When the address carries
//    one of those transient parameters the banners become toasts and the
//    parameters leave the URL, so a reload or a bookmark never replays
//    "Saved." Banners on a page WITHOUT such a parameter are state notices
//    (stale dashboard, closed options session) and stay inline.

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

  var TRANSIENT = ["flash", "err", "mwfills"];

  function flashKind(el) {
    var c = el.classList;
    if (c.contains("error") || c.contains("gex-failed")) return "error";
    if (c.contains("warn") || c.contains("deleted")) return "warn";
    if (c.contains("saved") || c.contains("created") || c.contains("moved") ||
        c.contains("analyzed") || c.contains("gex-refreshed")) return "success";
    return "info";
  }

  function bridgeFlashes() {
    var params = new URLSearchParams(location.search);
    if (!TRANSIENT.some(function (k) { return params.has(k); })) return;
    var root = document.querySelector("main") || document.body;
    root.querySelectorAll(".flash").forEach(function (el) {
      // A link inside the banner becomes the toast's action ("Open").
      var link = el.querySelector("a[href]");
      var action = null;
      if (link) {
        action = { label: link.textContent.replace(/\s+/g, " ").trim() || "Open", href: link.getAttribute("href") };
        link.remove();
      }
      var text = (el.textContent || "").replace(/\s+/g, " ").trim();
      var kind = flashKind(el);
      el.remove();
      if (!text || typeof window.toast !== "function") return;
      // Errors and warnings wait to be read; a long explanation too.
      var sticky = kind === "error" || kind === "warn" || text.length > 140;
      window.toast(text, { kind: kind, duration: sticky ? 0 : 6000, action: action });
    });
    TRANSIENT.forEach(function (k) { params.delete(k); });
    var qs = params.toString();
    try {
      history.replaceState(history.state, "", location.pathname + (qs ? "?" + qs : "") + location.hash);
    } catch (_) { /* sandboxed frame: the banner is gone either way */ }
  }
  bridgeFlashes();
})();
