// trade_sheet.js — keyboard navigation and lifecycle for the right-side
// trade-detail sheet. Activated on any page that includes the trade_sheet
// partial; quietly idles when there's no sheet on the page.
//
// Behaviors:
//   - Row click already wires HTMX hx-get → #trade-sheet-content; this
//     script reveals the sheet, focuses it, records the current trade ID,
//     and disables prev/next buttons at list boundaries.
//   - j / ↓ → next trade in the visible list
//     k / ↑ → previous
//     g / n / b → click corresponding tag radio (good / neutral / bad)
//     Esc → close
//     ⌘S / Ctrl-S inside the sheet → submit the journal form
//   - Shortcuts that conflict with typing (j/k/g/n/b) are suppressed when
//     focus is in an input/textarea/contenteditable.
//
// Host page may set `window.tradeSheetOnClose` for page-specific cleanup
// (e.g., /trades reloads when journal changed; /analytics just hides).

(function () {
  "use strict";

  function ready(fn) {
    if (document.readyState !== "loading") { fn(); }
    else { document.addEventListener("DOMContentLoaded", fn); }
  }

  ready(init);

  function init() {
    var backdrop = document.getElementById("trade-sheet-backdrop");
    if (!backdrop) return; // page doesn't host a sheet

    var state = {
      backdrop: backdrop,
      sheet: backdrop.querySelector(".trade-sheet"),
      content: document.getElementById("trade-sheet-content"),
      title: document.getElementById("trade-sheet-title"),
      prevBtn: backdrop.querySelector("[data-trade-sheet-prev]"),
      nextBtn: backdrop.querySelector("[data-trade-sheet-next]"),
      currentId: null,
    };

    bindRowClicks(state);
    bindControls(state);
    bindKeyboard(state);
    bindAfterSwap(state);
    bindBackdropClick(state);
    bindDeepLink(state);
  }

  // ---------- Row click → open sheet ----------

  function bindRowClicks(state) {
    document.addEventListener("click", function (e) {
      var trigger = e.target.closest && e.target.closest("[data-trade-id]");
      if (!trigger) return;
      // .trade-row rows: ignore clicks on nested controls so checkbox/etc.
      // keep their default. Anchors with data-trade-id are themselves the
      // trigger so we allow them through.
      if (trigger.classList.contains("trade-row")) {
        if (e.target.closest("input, button, label, a")) return;
      }
      var id = trigger.getAttribute("data-trade-id");
      if (!id) return;
      // HTMX on the trigger already fires the request into the sheet;
      // reveal without re-fetching.
      revealSheet(state, id);
    });
    // Keyboard: a focused row (rows carry tabindex="0") opens on Enter or
    // Space, the same click HTMX listens for, so no second code path.
    document.addEventListener("keydown", function (e) {
      if (e.key !== "Enter" && e.key !== " ") return;
      var row = e.target && e.target.closest && e.target.closest("tr.trade-row[data-trade-id]");
      if (!row || e.target !== row) return;
      e.preventDefault();
      row.click();
    });
  }

  // revealSheet shows the sheet for a trade ID without fetching content. Use
  // when HTMX (or another mechanism) will deliver the content separately.
  function revealSheet(state, id) {
    state.currentId = String(id);
    if (!state.backdrop.classList.contains("show")) {
      state.previouslyFocused = document.activeElement;
    }
    show(state);
    refreshNavButtons(state);
    focusSheet(state);
  }

  // navigateToTrade actively loads /trades/{id} into the sheet — for keyboard
  // nav and deep links where no row click happened.
  function navigateToTrade(state, id) {
    revealSheet(state, id);
    if (window.htmx && state.content) {
      window.htmx.ajax("GET", "/trades/" + id, "#trade-sheet-content");
    }
  }

  function focusSheet(state) {
    if (state.sheet && state.sheet.focus) {
      try { state.sheet.focus({ preventScroll: true }); } catch (_) { state.sheet.focus(); }
    }
  }

  function show(state) {
    state.backdrop.classList.add("show");
    state.backdrop.setAttribute("aria-hidden", "false");
    document.body.style.overflow = "hidden"; // prevent background scroll
  }

  function hide(state) {
    state.backdrop.classList.remove("show");
    state.backdrop.setAttribute("aria-hidden", "true");
    document.body.style.overflow = "";
    state.currentId = null;
    // Restore focus to the element that opened the sheet (the trade row, or
    // whatever the user activated). Falls through silently if that element
    // is gone from the DOM (e.g., after a paginate / filter).
    var prev = state.previouslyFocused;
    state.previouslyFocused = null;
    if (prev && typeof prev.focus === "function" && document.contains(prev)) {
      try { prev.focus({ preventScroll: true }); } catch (_) { prev.focus(); }
    }
  }

  // ---------- Prev / next / close buttons ----------

  function bindControls(state) {
    state.prevBtn && state.prevBtn.addEventListener("click", function () { step(state, -1); });
    state.nextBtn && state.nextBtn.addEventListener("click", function () { step(state, 1); });
    var closeBtn = state.backdrop.querySelector("[data-trade-sheet-close]");
    closeBtn && closeBtn.addEventListener("click", function () { close(state); });
  }

  function bindBackdropClick(state) {
    state.backdrop.addEventListener("click", function (e) {
      // Only when the click lands on the dim area itself, not the sheet.
      if (e.target === state.backdrop) close(state);
    });
  }

  function close(state) {
    if (typeof window.tradeSheetOnClose === "function") {
      // Page-specific cleanup may decide to leave the sheet open or reload.
      // If it returns false, abort the close.
      var keep = window.tradeSheetOnClose() === false;
      if (keep) return;
    }
    hide(state);
  }

  // ---------- Keyboard ----------

  function bindKeyboard(state) {
    document.addEventListener("keydown", function (e) {
      if (!state.backdrop.classList.contains("show")) return;
      var meta = e.metaKey || e.ctrlKey;

      if (e.key === "Escape") {
        e.preventDefault();
        close(state);
        return;
      }

      // Focus trap: keep Tab within the sheet while it claims aria-modal=true.
      if (e.key === "Tab") {
        var focusables = state.sheet.querySelectorAll(
          'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
        );
        if (focusables.length === 0) {
          // Content still loading ("loading…" has no focusables): keep the
          // trap engaged instead of letting Tab escape behind the modal.
          e.preventDefault();
          state.sheet.focus();
          return;
        }
        var first = focusables[0];
        var last = focusables[focusables.length - 1];
        var active = document.activeElement;
        if (e.shiftKey && (active === first || !state.sheet.contains(active))) {
          last.focus();
          e.preventDefault();
        } else if (!e.shiftKey && active === last) {
          first.focus();
          e.preventDefault();
        }
        return;
      }

      if (meta && (e.key === "s" || e.key === "S")) {
        var form = state.content && state.content.querySelector("form[hx-post], form[action]");
        if (form) {
          e.preventDefault();
          form.requestSubmit ? form.requestSubmit() : form.submit();
        }
        return;
      }

      if (isTyping(e.target)) return; // don't intercept while editing

      switch (e.key) {
        case "j":
        case "ArrowDown":
          e.preventDefault();
          step(state, 1);
          return;
        case "k":
        case "ArrowUp":
          e.preventDefault();
          step(state, -1);
          return;
        case "g":
          e.preventDefault();
          chooseTag(state, "good");
          return;
        case "n":
          e.preventDefault();
          chooseTag(state, "neutral");
          return;
        case "b":
          e.preventDefault();
          chooseTag(state, "bad");
          return;
      }
    });
  }

  function isTyping(el) {
    if (!el) return false;
    var tag = el.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
    if (el.isContentEditable) return true;
    return false;
  }

  function chooseTag(state, tag) {
    var radio = state.content && state.content.querySelector('input[type=radio][name=tag][value="' + tag + '"]');
    if (!radio) return;
    radio.checked = true;
    radio.dispatchEvent(new Event("change", { bubbles: true }));
  }

  // ---------- Navigation between trades ----------

  function collectTradeIds() {
    var ids = [];
    var seen = {};
    document.querySelectorAll("[data-trade-id]").forEach(function (el) {
      var id = String(el.getAttribute("data-trade-id"));
      if (id && !seen[id]) { seen[id] = true; ids.push(id); }
    });
    return ids;
  }

  function step(state, delta) {
    if (!state.currentId) return;
    var ids = collectTradeIds();
    var idx = ids.indexOf(state.currentId);
    if (idx < 0) return;
    var next = idx + delta;
    if (next < 0 || next >= ids.length) return;
    navigateToTrade(state, ids[next]);
  }

  function refreshNavButtons(state) {
    var ids = collectTradeIds();
    var idx = ids.indexOf(String(state.currentId));
    var atStart = idx <= 0;
    var atEnd = idx < 0 || idx >= ids.length - 1;
    if (state.prevBtn) state.prevBtn.disabled = atStart;
    if (state.nextBtn) state.nextBtn.disabled = atEnd;
  }

  // ---------- After HTMX swap ----------

  function bindAfterSwap(state) {
    document.body.addEventListener("htmx:afterSwap", function (e) {
      if (!state.content) return;
      if (e.target === state.content || state.content.contains(e.target)) {
        if (state.currentId) state.content.dataset.trade = state.currentId;
        refreshNavButtons(state);
      }
    });
  }

  // ---------- Deep link (?modal=N or ?sheet=N) ----------

  function bindDeepLink(state) {
    var params = new URLSearchParams(location.search);
    var id = params.get("sheet") || params.get("modal");
    if (!id) return;
    // Defer until other DOM is ready so trade-row collection works.
    setTimeout(function () { navigateToTrade(state, id); }, 30);
  }
})();
