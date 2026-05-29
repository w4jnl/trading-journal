// Journal editor — enhances any [data-je] block authored by
// templates/_journal_editor.html. Adds:
//   - debounced server-rendered preview
//   - paste-image and drop-image upload (POST /journal/attachments)
//   - toolbar buttons (bold/italic/code/heading/bullet) and ⌘B/⌘I shortcuts
//   - markdown shortcuts: pressing Enter inside "- " or "N. " continues the list
//   - templates dropdown (fetches /journal/templates?scope=...)
//   - ⌘P toggles between split + single-pane
//   - Esc closes any popup
// Nothing here is required for the form to submit — the underlying textarea
// name="body" is the form's source of truth.

(function () {
  "use strict";

  // Initialize on DOMContentLoaded; re-init for elements inserted later
  // (e.g., HTMX swap of the trade-detail sheet).
  function ready(fn) {
    if (document.readyState !== "loading") { fn(); }
    else { document.addEventListener("DOMContentLoaded", fn); }
  }

  ready(function () {
    document.querySelectorAll("[data-je]").forEach(init);
    // HTMX integration: after-swap, scan the swapped tree for new editors.
    document.body.addEventListener("htmx:afterSwap", function (e) {
      var root = e.target || e.detail.target || document;
      if (root.querySelectorAll) {
        root.querySelectorAll("[data-je]").forEach(init);
      }
    });
  });

  function init(root) {
    if (root.__jeInited) return;
    root.__jeInited = true;

    var ta = root.querySelector("[data-je-input]");
    var pv = root.querySelector("[data-je-preview]");
    var status = root.querySelector("[data-je-status]");
    if (!ta || !pv) return;

    var ctx = {
      root: root,
      ta: ta,
      pv: pv,
      status: status,
      scope: root.dataset.scope || "",
      entryId: root.dataset.entryId || "0",
      previewUrl: root.dataset.previewUrl || "/journal/preview",
      attachUrl: root.dataset.attachUrl || "/journal/attachments",
      lastBody: ta.value,
      saved: ta.value,
    };

    // Initial preview render.
    schedulePreview(ctx, 0);

    bindInput(ctx);
    bindToolbar(ctx);
    bindShortcuts(ctx);
    bindPasteAndDrop(ctx);
  }

  // ---------- Preview (debounced server render) ----------

  function schedulePreview(ctx, delay) {
    clearTimeout(ctx._previewTimer);
    ctx._previewTimer = setTimeout(function () { renderPreview(ctx); }, delay);
  }

  function renderPreview(ctx) {
    var body = ctx.ta.value;
    if (body === ctx._lastRendered) return; // no-op
    ctx._lastRendered = body;
    var form = new URLSearchParams();
    form.set("body", body);
    fetch(ctx.previewUrl, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded; charset=utf-8" },
      body: form.toString(),
    }).then(function (r) {
      if (!r.ok) throw new Error("preview HTTP " + r.status);
      return r.text();
    }).then(function (html) {
      ctx.pv.innerHTML = html;
    }).catch(function (err) {
      ctx.pv.innerHTML = '<p class="journal-editor__preview-empty">Preview unavailable: ' + escapeHtml(String(err)) + "</p>";
    });
  }

  // ---------- Input change tracking ----------

  function bindInput(ctx) {
    ctx.ta.addEventListener("input", function () {
      schedulePreview(ctx, 250);
      setStatus(ctx, ctx.ta.value === ctx.saved ? "" : "Unsaved");
    });
    // Suppress browser-native form-reset re-render bug.
    ctx.ta.form && ctx.ta.form.addEventListener("submit", function () {
      ctx.saved = ctx.ta.value;
      setStatus(ctx, "");
    });
  }

  function setStatus(ctx, text, kind) {
    if (!ctx.status) return;
    ctx.status.textContent = text || "";
    ctx.status.className = "je-status" + (kind ? " je-status--" + kind : "");
  }

  // ---------- Toolbar buttons ----------

  function bindToolbar(ctx) {
    ctx.root.querySelectorAll("[data-je-action]").forEach(function (btn) {
      btn.addEventListener("click", function (e) {
        e.preventDefault();
        var act = btn.dataset.jeAction;
        switch (act) {
          case "toggle-preview":
            ctx.root.classList.toggle("journal-editor--single");
            var on = !ctx.root.classList.contains("journal-editor--single");
            btn.setAttribute("aria-pressed", on ? "true" : "false");
            break;
          case "wrap":
            wrapSelection(ctx, btn.dataset.jeBefore || "", btn.dataset.jeAfter || "");
            break;
          case "insert":
            insertAtLineStart(ctx, btn.dataset.jeTemplate || "");
            break;
          case "image":
            promptImageFile(ctx);
            break;
          case "templates":
            toggleTemplates(ctx, btn);
            break;
          case "recall":
            toggleRecall(ctx, btn);
            break;
        }
      });
    });
  }

  // ---------- Keyboard shortcuts ----------

  function bindShortcuts(ctx) {
    ctx.ta.addEventListener("keydown", function (e) {
      var meta = e.metaKey || e.ctrlKey;
      if (meta && !e.shiftKey && !e.altKey) {
        switch (e.key.toLowerCase()) {
          case "b": e.preventDefault(); wrapSelection(ctx, "**", "**"); return;
          case "i": e.preventDefault(); wrapSelection(ctx, "_",  "_");  return;
          case "p":
            e.preventDefault();
            ctx.root.classList.toggle("journal-editor--single");
            return;
          case "s":
            // Let the form's Save button handle it; just submit.
            if (ctx.ta.form) {
              e.preventDefault();
              ctx.ta.form.requestSubmit ? ctx.ta.form.requestSubmit() : ctx.ta.form.submit();
            }
            return;
        }
      }
      if (e.key === "Enter" && !e.shiftKey && !meta) {
        // Continue a markdown list on Enter.
        var line = currentLine(ctx.ta);
        var bullet = line.text.match(/^(\s*)([-*])\s+/);
        var numbered = line.text.match(/^(\s*)(\d+)\.\s+/);
        if (bullet) {
          // Empty list item terminates the list.
          if (line.text.length === bullet[0].length) {
            e.preventDefault();
            replaceLine(ctx.ta, line, "");
            return;
          }
          e.preventDefault();
          insertAtCursor(ctx.ta, "\n" + bullet[1] + bullet[2] + " ");
          schedulePreview(ctx, 100);
          return;
        }
        if (numbered) {
          if (line.text.length === numbered[0].length) {
            e.preventDefault();
            replaceLine(ctx.ta, line, "");
            return;
          }
          e.preventDefault();
          var next = String(parseInt(numbered[2], 10) + 1);
          insertAtCursor(ctx.ta, "\n" + numbered[1] + next + ". ");
          schedulePreview(ctx, 100);
          return;
        }
      }
    });
  }

  function currentLine(ta) {
    var pos = ta.selectionStart;
    var value = ta.value;
    var start = value.lastIndexOf("\n", pos - 1) + 1;
    var end = value.indexOf("\n", pos);
    if (end < 0) end = value.length;
    return { start: start, end: end, text: value.slice(start, end) };
  }

  function replaceLine(ta, line, replacement) {
    ta.setRangeText(replacement, line.start, line.end, "end");
    ta.dispatchEvent(new Event("input", { bubbles: true }));
  }

  function insertAtCursor(ta, text) {
    var s = ta.selectionStart, e = ta.selectionEnd;
    ta.setRangeText(text, s, e, "end");
    ta.dispatchEvent(new Event("input", { bubbles: true }));
  }

  function wrapSelection(ctx, before, after) {
    var ta = ctx.ta;
    var s = ta.selectionStart, e = ta.selectionEnd;
    var selected = ta.value.slice(s, e);
    var insert = before + selected + after;
    ta.setRangeText(insert, s, e, "end");
    if (!selected) {
      // Place caret between the markers.
      var caret = s + before.length;
      ta.setSelectionRange(caret, caret);
    }
    ta.focus();
    ta.dispatchEvent(new Event("input", { bubbles: true }));
  }

  function insertAtLineStart(ctx, prefix) {
    var ta = ctx.ta;
    var line = currentLine(ta);
    var newText = prefix + line.text;
    ta.setRangeText(newText, line.start, line.end, "end");
    ta.focus();
    ta.dispatchEvent(new Event("input", { bubbles: true }));
  }

  // ---------- Image upload (paste + drop + button) ----------

  function bindPasteAndDrop(ctx) {
    ctx.ta.addEventListener("paste", function (e) {
      var items = e.clipboardData && e.clipboardData.items;
      if (!items) return;
      for (var i = 0; i < items.length; i++) {
        if (items[i].type && items[i].type.indexOf("image/") === 0) {
          e.preventDefault();
          var file = items[i].getAsFile();
          if (file) uploadAndInsert(ctx, file);
          return;
        }
      }
    });

    var stop = function (e) { e.preventDefault(); e.stopPropagation(); };
    ["dragenter", "dragover"].forEach(function (ev) {
      ctx.root.addEventListener(ev, function (e) {
        stop(e);
        ctx.root.classList.add("is-dragover");
      });
    });
    ["dragleave", "dragend", "drop"].forEach(function (ev) {
      ctx.root.addEventListener(ev, function (e) {
        if (ev === "drop") stop(e);
        ctx.root.classList.remove("is-dragover");
      });
    });
    ctx.root.addEventListener("drop", function (e) {
      stop(e);
      var files = e.dataTransfer && e.dataTransfer.files;
      if (!files || files.length === 0) return;
      for (var i = 0; i < files.length; i++) {
        if (files[i].type.indexOf("image/") === 0) {
          uploadAndInsert(ctx, files[i]);
        }
      }
    });
  }

  function promptImageFile(ctx) {
    var input = document.createElement("input");
    input.type = "file";
    input.accept = "image/*";
    input.addEventListener("change", function () {
      if (input.files && input.files[0]) uploadAndInsert(ctx, input.files[0]);
    });
    input.click();
  }

  function uploadAndInsert(ctx, file) {
    setStatus(ctx, "Uploading…", "saving");
    var fd = new FormData();
    fd.append("file", file);
    fd.append("scope", ctx.scope);
    fd.append("entry_id", ctx.entryId);

    fetch(ctx.attachUrl, { method: "POST", body: fd })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
      .then(function (res) {
        if (!res.ok) throw new Error((res.body && res.body.error) || "upload failed");
        var name = (file.name || "image").replace(/[\[\]]/g, "");
        var md = "![" + name + "](" + res.body.url + ")";
        insertAtCursor(ctx.ta, md + "\n");
        setStatus(ctx, "Image inserted", "saved");
        setTimeout(function () {
          if (ctx.status && ctx.status.textContent === "Image inserted") setStatus(ctx, "");
        }, 2500);
        if (window.toast) window.toast("Image uploaded", { kind: "success", duration: 2500 });
      })
      .catch(function (err) {
        setStatus(ctx, "Upload failed", "error");
        if (window.toast) window.toast("Image upload failed: " + err.message, { kind: "error" });
      });
  }

  // mountDropdown portals `box` to <body> and positions it (fixed) under the
  // trigger `button`. Rendering inside the editor subtree hit two problems:
  // a <button> flattens block children (overlap), and the editor root's
  // `overflow: hidden` + inner stacking contexts clipped the menu's first
  // row. Appending to <body> sidesteps both — nothing between :root and the
  // box can clip or overlap it.
  function mountDropdown(button, box) {
    document.body.appendChild(box);
    positionDropdown(button, box);
    box.scrollTop = 0;
    // Reposition on scroll / resize while open (the box is position:fixed,
    // so it doesn't track the button on its own). Self-removing: once the
    // close handler detaches the box, the next event tears the listeners
    // down.
    var reposition = function () {
      if (!box.isConnected) {
        window.removeEventListener("scroll", reposition, true);
        window.removeEventListener("resize", reposition);
        return;
      }
      positionDropdown(button, box);
    };
    window.addEventListener("scroll", reposition, true);
    window.addEventListener("resize", reposition);
  }

  // positionDropdown places a position:fixed dropdown just below its trigger
  // button, right-aligned to it, clamped into the viewport.
  function positionDropdown(button, box) {
    var r = button.getBoundingClientRect();
    box.style.top = Math.round(r.bottom + 4) + "px";
    var width = box.offsetWidth || 260;
    var left = Math.round(r.right - width);
    var pad = 8;
    if (left < pad) left = pad;
    var maxLeft = window.innerWidth - width - pad;
    if (left > maxLeft) left = Math.max(pad, maxLeft);
    box.style.left = left + "px";
  }

  // ---------- Templates dropdown ----------

  function toggleTemplates(ctx, button) {
    // Menus live in <body>, so look there. At most one is open.
    var existing = document.querySelector(".je-templates");
    if (existing) { existing.remove(); return; }
    fetchTemplates(ctx).then(function (templates) {
      var box = document.createElement("div");
      box.className = "je-templates";
      if (!templates || templates.length === 0) {
        var empty = document.createElement("div");
        empty.className = "je-templates-item";
        empty.style.color = "var(--fg-muted)";
        empty.textContent = "No templates available";
        box.appendChild(empty);
      }
      templates.forEach(function (tpl) {
        var item = document.createElement("button");
        item.type = "button";
        item.className = "je-templates-item";
        item.textContent = tpl.name + (tpl.builtin ? "" : "  ·  custom");
        item.addEventListener("click", function () {
          applyTemplate(ctx, tpl.body);
          box.remove();
        });
        box.appendChild(item);
      });
      mountDropdown(button, box);
      // Close on outside click / Esc. box lives in <body>, so check both
      // the trigger and the box explicitly.
      function close(e) {
        if (e.type === "keydown" && e.key !== "Escape") return;
        if (e.type === "click" && (button.contains(e.target) || box.contains(e.target))) return;
        box.remove();
        document.removeEventListener("click", close, true);
        document.removeEventListener("keydown", close);
      }
      setTimeout(function () {
        document.addEventListener("click", close, true);
        document.addEventListener("keydown", close);
      }, 0);
    });
  }

  function fetchTemplates(ctx) {
    if (ctx._templates) return Promise.resolve(ctx._templates);
    var url = "/journal/templates";
    if (ctx.scope) url += "?scope=" + encodeURIComponent(ctx.scope);
    return fetch(url).then(function (r) { return r.json(); }).then(function (j) {
      ctx._templates = (j && j.templates) || [];
      return ctx._templates;
    }).catch(function () { return []; });
  }

  function applyTemplate(ctx, body) {
    var ta = ctx.ta;
    if (ta.value.trim() === "") {
      ta.value = body;
    } else if (window.confirm("Replace current content with this template?")) {
      ta.value = body;
    } else {
      // Append at end with a separator.
      ta.value = ta.value.replace(/\s+$/, "") + "\n\n" + body;
    }
    ta.dispatchEvent(new Event("input", { bubbles: true }));
    ta.focus();
  }

  // ---------- Recall (prior entries of same scope) ----------

  function toggleRecall(ctx, button) {
    var existing = document.querySelector(".je-recall");
    if (existing) { existing.remove(); return; }
    fetchRecent(ctx).then(function (entries) {
      var box = document.createElement("div");
      box.className = "je-recall";
      if (!entries || entries.length === 0) {
        var empty = document.createElement("div");
        empty.className = "je-recall-empty";
        empty.textContent = "No prior " + (ctx.scope || "entries") + " entries yet.";
        box.appendChild(empty);
      }
      entries.forEach(function (e) {
        // A <div role=button>, NOT a <button>: a real button won't lay out
        // multi-line block children reliably (WebKit clips flex buttons at
        // the top and ignores block child heights), which is what made the
        // date overlap / clip. A div lays the date + snippet out normally.
        var item = document.createElement("div");
        item.className = "je-recall-item";
        item.setAttribute("role", "button");
        item.tabIndex = 0;
        var d = document.createElement("div");
        d.className = "je-recall-date";
        d.textContent = e.date_label;
        if (e.tag) {
          var tagBadge = document.createElement("span");
          tagBadge.style.marginLeft = "6px";
          tagBadge.style.fontSize = "10px";
          tagBadge.style.color = e.tag === "good" ? "var(--pos)" : e.tag === "bad" ? "var(--neg)" : "var(--fg-subtle)";
          tagBadge.textContent = "· " + e.tag;
          d.appendChild(tagBadge);
        }
        item.appendChild(d);
        var s = document.createElement("div");
        s.className = "je-recall-snippet";
        s.textContent = e.snippet;
        item.appendChild(s);
        var activate = function () {
          showPeek(ctx, e);
          box.remove();
        };
        item.addEventListener("click", activate);
        item.addEventListener("keydown", function (ev) {
          if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); activate(); }
        });
        box.appendChild(item);
      });
      mountDropdown(button, box);
      function close(e) {
        if (e.type === "keydown" && e.key !== "Escape") return;
        if (e.type === "click" && (button.contains(e.target) || box.contains(e.target))) return;
        box.remove();
        document.removeEventListener("click", close, true);
        document.removeEventListener("keydown", close);
      }
      setTimeout(function () {
        document.addEventListener("click", close, true);
        document.addEventListener("keydown", close);
      }, 0);
    });
  }

  function fetchRecent(ctx) {
    var url = "/journal/recent?scope=" + encodeURIComponent(ctx.scope || "day");
    if (ctx.entryId && ctx.entryId !== "0") {
      url += "&exclude_id=" + encodeURIComponent(ctx.entryId);
    }
    return fetch(url).then(function (r) { return r.json(); }).then(function (j) {
      return (j && j.entries) || [];
    }).catch(function () { return []; });
  }

  function showPeek(ctx, entry) {
    // Remove any existing peek.
    var prev = ctx.root.querySelector(".je-peek");
    if (prev) prev.remove();

    var peek = document.createElement("div");
    peek.className = "je-peek";

    var header = document.createElement("div");
    header.className = "je-peek-header";
    var title = document.createElement("span");
    title.className = "je-peek-title";
    title.textContent = "Recall · " + entry.date_label;
    header.appendChild(title);
    if (entry.tag) {
      var tag = document.createElement("span");
      tag.style.fontSize = "11px";
      tag.style.color = "var(--fg-muted)";
      tag.textContent = " (" + entry.tag + ")";
      header.appendChild(tag);
    }
    var close = document.createElement("button");
    close.type = "button";
    close.className = "je-peek-close";
    close.setAttribute("aria-label", "Close (Esc)");
    close.textContent = "×";
    close.addEventListener("click", function () { peek.remove(); });
    header.appendChild(close);
    peek.appendChild(header);

    var body = document.createElement("div");
    body.className = "je-peek-body";
    body.innerHTML = '<p class="journal-editor__preview-empty">Rendering…</p>';
    peek.appendChild(body);
    ctx.root.appendChild(peek);

    // Server-render the body to match the live-preview format.
    var form = new URLSearchParams();
    form.set("body", entry.body || "");
    fetch(ctx.previewUrl, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded; charset=utf-8" },
      body: form.toString(),
    }).then(function (r) { return r.text(); }).then(function (html) {
      body.innerHTML = html;
    }).catch(function (err) {
      body.innerHTML = '<p class="journal-editor__preview-empty">Could not render: ' + escapeHtml(String(err)) + "</p>";
    });

    // Esc closes the peek (only when peek itself is open and editor focused).
    function onKey(e) {
      if (e.key === "Escape" && ctx.root.contains(peek)) {
        e.stopPropagation();
        peek.remove();
        document.removeEventListener("keydown", onKey, true);
      }
    }
    document.addEventListener("keydown", onKey, true);
  }

  // ---------- Helpers ----------

  function escapeHtml(s) {
    return s.replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }
})();
