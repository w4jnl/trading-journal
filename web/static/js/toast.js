// Toast notifications — small top-right banners that auto-dismiss.
// Used as a less intrusive replacement for the inline .flash divs and as
// feedback for client-side actions (auto-save, copy-to-clipboard, etc).
//
// Usage from anywhere:
//   window.toast("Saved");
//   window.toast("Could not upload", { kind: "error" });
//   window.toast("Imported 42 trades", { kind: "success", duration: 6000 });
//
// `kind` is one of: success | error | warn | info (default).
// `duration` is ms before auto-dismiss (default 4000). 0 keeps it open.
(function () {
  function ensureStack() {
    var stack = document.getElementById("toast-stack");
    if (stack) return stack;
    stack = document.createElement("div");
    stack.id = "toast-stack";
    stack.setAttribute("aria-live", "polite");
    document.body.appendChild(stack);
    return stack;
  }

  function dismiss(node) {
    if (!node || node.classList.contains("toast--leaving")) return;
    node.classList.add("toast--leaving");
    // Match the CSS animation duration; remove regardless if a transition
    // gets cancelled (e.g., user navigates).
    var done = false;
    var remove = function () { if (!done) { done = true; node.remove(); } };
    node.addEventListener("animationend", remove, { once: true });
    setTimeout(remove, 250);
  }

  function show(message, opts) {
    opts = opts || {};
    var kind = opts.kind || "info";
    var duration = typeof opts.duration === "number" ? opts.duration : 4000;
    var stack = ensureStack();

    var node = document.createElement("div");
    node.className = "toast toast--" + kind;
    node.setAttribute("role", kind === "error" ? "alert" : "status");

    var text = document.createElement("span");
    text.textContent = message;
    node.appendChild(text);

    var close = document.createElement("button");
    close.type = "button";
    close.className = "toast-close";
    close.setAttribute("aria-label", "Dismiss");
    close.textContent = "×";
    close.addEventListener("click", function (e) {
      e.stopPropagation();
      dismiss(node);
    });
    node.appendChild(close);

    node.addEventListener("click", function () { dismiss(node); });

    stack.appendChild(node);

    if (duration > 0) {
      setTimeout(function () { dismiss(node); }, duration);
    }
    return node;
  }

  window.toast = show;
})();
