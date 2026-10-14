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
//   - e → open the note editor (Add note / Edit) and focus it
//   - The row's journal icon ([data-journal-open]) opens the sheet with the
//     note editor unfolded (/trades/{id}?journal=1); handled in the capture
//     phase so the row's own hx-get does not fire as well.
//   - Shortcuts that conflict with typing (j/k/g/n/b/e) are suppressed when
//     focus is in an input/textarea/contenteditable.
//   - History: opening the sheet pushes a history entry (the URL stays as
//     it is, filters included); opening a trade from inside a set sheet
//     pushes another. The browser's Back and Esc therefore return to the
//     set, then close the sheet, and the page underneath never changes.
//     j/k replace the entry instead of pushing, so Back still closes.
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

    state = {
      backdrop: backdrop,
      sheet: backdrop.querySelector(".trade-sheet"),
      content: document.getElementById("trade-sheet-content"),
      title: document.getElementById("trade-sheet-title"),
      prevBtn: backdrop.querySelector("[data-trade-sheet-prev]"),
      nextBtn: backdrop.querySelector("[data-trade-sheet-next]"),
      backBtn: backdrop.querySelector("[data-trade-sheet-back]"),
      currentId: null,
      pushed: 0, // history entries the sheet owns right now
      depth: 0,  // nested opens (a trade from inside a set) beyond the first
    };

    bindRowClicks(state);
    bindControls(state);
    bindKeyboard(state);
    bindAfterSwap(state);
    bindBackdropClick(state);
    bindPlanMirror(state);
    bindHistory(state);
    bindDeepLink(state);
  }

  // ---------- Row click → open sheet ----------

  function bindRowClicks(state) {
    // Journal icon: capture phase, before the row's htmx listener sees the
    // click, so only the note request goes out.
    document.addEventListener("click", function (e) {
      var btn = e.target.closest && e.target.closest("[data-journal-open]");
      if (!btn) return;
      e.stopPropagation();
      e.preventDefault();
      remember(state, btn.getAttribute("data-journal-open"), false);
      navigateToTrade(state, btn.getAttribute("data-journal-open"), "?journal=1");
    }, true);
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
      // reveal without re-fetching. A row inside the sheet (a set's
      // member) opens on top of what is shown: one more step to come back.
      var nested = state.backdrop.classList.contains("show") && state.content && state.content.contains(trigger);
      remember(state, id, nested);
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

  // ---------- History ----------

  // remember records the step in the browser history before a sheet opens:
  // a fresh open pushes one entry, a nested open (a trade from inside a
  // set) pushes another, a sibling (j/k) replaces the current one. The
  // URL is left as it is, so filters survive and nothing reopens on reload.
  function remember(state, id, nested) {
    if (!window.history || !history.pushState) return;
    var entry = { tjSheet: String(id) };
    var open = state.backdrop.classList.contains("show");
    if (!open) {
      history.pushState(entry, "", location.href);
      state.pushed = 1;
      state.depth = 0;
    } else if (nested) {
      history.pushState(entry, "", location.href);
      state.pushed++;
      state.depth++;
    } else if (state.pushed > 0) {
      history.replaceState(entry, "", location.href);
    }
    updateBackButton(state);
  }

  function updateBackButton(state) {
    if (state.backBtn) state.backBtn.hidden = state.depth <= 0;
  }

  // goBack steps one level up: the parent sheet when nested, else closed.
  function goBack(state) {
    if (state.pushed > 0) {
      history.back(); // the popstate handler does the rest
      return;
    }
    close(state);
  }

  function bindHistory(state) {
    state.backBtn && state.backBtn.addEventListener("click", function () { goBack(state); });
    window.addEventListener("popstate", function (e) {
      if (!state.backdrop.classList.contains("show")) return;
      if (e.state && e.state.tjSheet && state.pushed > 1) {
        // Up one level: the parent sheet (a set, or the trade this one was
        // opened from).
        state.pushed--;
        state.depth = Math.max(0, state.depth - 1);
        updateBackButton(state);
        navigateToTrade(state, e.state.tjSheet);
        return;
      }
      // Back past the first open: the sheet closes, the page is as it was.
      state.pushed = 0;
      state.depth = 0;
      updateBackButton(state);
      if (typeof window.tradeSheetOnClose === "function") window.tradeSheetOnClose();
      hide(state);
    });
  }

  // navigateToTrade actively loads /trades/{id} into the sheet — for keyboard
  // nav and deep links where no row click happened.
  // sheetURL is the sheet content for a row id: a trade, a copy set
  // ("set:N" rows of the sets view) or an open position ("pos:<account>:<symbol>"
  // rows of the open strip; the symbol may itself hold colons).
  function sheetURL(id) {
    id = String(id);
    if (id.indexOf("set:") === 0) return "/trades/sets/" + id.slice(4);
    if (id.indexOf("pos:") === 0) {
      var rest = id.slice(4), cut = rest.indexOf(":");
      if (cut > 0) return "/positions/" + rest.slice(0, cut) + "/" + encodeURIComponent(rest.slice(cut + 1));
    }
    return "/trades/" + id;
  }

  function navigateToTrade(state, id, query) {
    revealSheet(state, id);
    if (window.htmx && state.content) {
      window.htmx.ajax("GET", sheetURL(id) + (query || ""), "#trade-sheet-content");
    }
  }

  // openNote unfolds the note editor in the current sheet (the Add note /
  // Edit button of journal_view.js) and focuses it; a no-op without one.
  function openNote(state) {
    if (!state.content) return;
    var form = state.content.querySelector("form.journal-form");
    if (form && form.hidden) {
      var edit = state.content.querySelector("[data-journal-edit]");
      if (edit) edit.click();
    }
    var ta = state.content.querySelector("form.journal-form textarea[data-je-input]");
    if (ta) {
      ta.focus();
      if (ta.scrollIntoView) ta.scrollIntoView({ block: "center" });
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

  // ---------- Plan row: stop <-> risk mirror ----------
  // Typing a stop fills the risk, typing a risk fills the stop (losing side
  // of the entry, snapped to the tick), from the instrument facts on the
  // form. The server redoes the same math on save, so no-JS still works.
  function bindPlanMirror(state) {
    document.addEventListener("input", function (e) {
      var input = e.target;
      if (!input || !input.form || !input.form.classList || !input.form.classList.contains("trade-plan")) return;
      var f = input.form;
      var entry = parseFloat(f.dataset.planEntry), qty = parseFloat(f.dataset.planQty);
      var pv = parseFloat(f.dataset.planPointValue), tick = parseFloat(f.dataset.planTick);
      if (!(entry > 0) || !(qty > 0) || !(pv > 0)) return;
      var stop = f.elements.stop, risk = f.elements.risk;
      if (!stop || !risk) return;
      if (input === stop) {
        var sv = parseFloat(stop.value.replace(",", "."));
        if (!(sv > 0)) { if (stop.value.trim() === "") risk.value = ""; return; }
        risk.value = (Math.abs(entry - sv) * pv * qty).toFixed(2);
      } else if (input === risk) {
        var rv = parseFloat(risk.value.replace(",", "."));
        if (!(rv > 0)) { if (risk.value.trim() === "") stop.value = ""; return; }
        var dist = rv / (pv * qty);
        if (tick > 0) dist = Math.round(dist / tick) * tick;
        if (!(dist > 0)) return;
        var sp = f.dataset.planDirection === "short" ? entry + dist : entry - dist;
        stop.value = String(Math.round(sp * 1e6) / 1e6);
      }
    });
  }

  function bindBackdropClick(state) {
    state.backdrop.addEventListener("click", function (e) {
      // Only when the click lands on the dim area itself, not the sheet.
      if (e.target === state.backdrop) close(state);
    });
  }

  function close(state) {
    if (state.pushed > 0) {
      // Unwind every entry the sheet pushed; the popstate handler hides it
      // (and runs the page's close hook) once the page's own entry is back.
      var n = state.pushed;
      state.pushed = 0;
      state.depth = 0;
      updateBackButton(state);
      history.go(-n);
      return;
    }
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
        if (state.depth > 0) { goBack(state); return; }
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
        case "e":
          e.preventDefault();
          openNote(state);
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

  var state; // set by init; collectTradeIds needs the sheet to skip its own rows
  function collectTradeIds() {
    var ids = [];
    var seen = {};
    document.querySelectorAll("[data-trade-id]").forEach(function (el) {
      if (el.hidden || (el.offsetParent === null && el.tagName === "TR")) return; // folded set members
      if (state && state.content && state.content.contains(el)) return; // rows inside the sheet (a set's legs)
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
    remember(state, ids[next], false);
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
        // Opened from the row's journal icon: the editor is already shown;
        // put the cursor in it.
        if (state.content.querySelector("form.journal-form[data-journal-open]")) openNote(state);
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

// Sheet title: the trade detail partial carries it as data (a hidden
// .sheet-title-src marker) instead of an inline script, so a CSP without
// 'unsafe-inline' can run the page. Applied after every swap into the sheet.
(function () {
  "use strict";
  document.body.addEventListener("htmx:afterSwap", function (e) {
    var content = e.target;
    if (!content || content.id !== "trade-sheet-content") return;
    var src = content.querySelector("[data-sheet-title]");
    var title = document.getElementById("trade-sheet-title");
    if (src && title) title.textContent = src.getAttribute("data-sheet-title");
  });
})();
