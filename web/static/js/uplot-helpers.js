// uplot-helpers.js - shared plumbing for the uPlot charts on /dashboard,
// /trades, and /analytics (previously copy-pasted per page). Loaded by the
// "uplot_assets" template partial immediately after the uPlot library, so
// window.uPlot is guaranteed here.

(function () {
  "use strict";

  // Theme-aware colors read from the CSS tokens so charts follow dark/light.
  function colors() {
    var cs = getComputedStyle(document.documentElement);
    return {
      accent: cs.getPropertyValue("--accent").trim() || "#4f46e5",
      pos: cs.getPropertyValue("--pos").trim() || "#16a34a",
      neg: cs.getPropertyValue("--neg").trim() || "#dc2626",
      grid: cs.getPropertyValue("--border").trim() || "#e4e4e7",
      label: cs.getPropertyValue("--fg-muted").trim() || "#71717a",
    };
  }

  // rgba("#abc"|"#aabbcc", a) - tolerates rgb()/rgba() input untouched.
  function rgba(hex, a) {
    if (hex.indexOf("rgb") === 0) return hex;
    var h = hex.replace("#", "");
    if (h.length === 3) h = h.split("").map(function (x) { return x + x; }).join("");
    var n = parseInt(h, 16);
    return "rgba(" + ((n >> 16) & 255) + ", " + ((n >> 8) & 255) + ", " + (n & 255) + ", " + a + ")";
  }

  // Compact axis money: "52,000" stays, "152,000" -> "152K", 1.25M etc. Keeps
  // 6-7 digit P&L/equity numbers inside a reasonable Y-axis gutter.
  function fmtMoneyShort(v) {
    if (v == null || Number.isNaN(v)) return "";
    var abs = Math.abs(v);
    if (abs >= 1e6) return (v / 1e6).toFixed(2).replace(/\.0+$/, "") + "M";
    if (abs >= 1e5) return Math.round(v / 1e3) + "K";
    return v.toLocaleString(undefined, { maximumFractionDigits: 0 });
  }

  function fmtMoneyFull(v) {
    if (v == null || Number.isNaN(v)) return "—";
    return v.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  // Standard two-axis config; moneyAxes widens the Y gutter (uPlot's default
  // 50px crops 6-7 digit values) and formats ticks compactly.
  function axes(c) {
    return [
      { stroke: c.label, grid: { stroke: c.grid, width: 1 } },
      { stroke: c.label, grid: { stroke: c.grid, width: 1 } },
    ];
  }
  function moneyAxes(c) {
    return [
      { stroke: c.label, grid: { stroke: c.grid, width: 1 } },
      {
        stroke: c.label,
        grid: { stroke: c.grid, width: 1 },
        size: 70,
        values: function (u, splits) { return splits.map(fmtMoneyShort); },
      },
    ];
  }

  // Rebuild hook: charts re-render on theme toggle (new token colors) and on
  // resize (uPlot sizes are fixed at construction).
  //
  // Two registration styles:
  //  - onRebuild(fn): append-only, for scripts that run exactly once per full
  //    page load (e.g. the gex page).
  //  - rebuildKey(key, fn): keyed and idempotent - re-registering the same
  //    key REPLACES the previous fn. Required for scripts inside live-refresh
  //    fragment regions, which re-execute on every swap: without the key each
  //    swap would stack another listener whose stale closure rebuilds a
  //    duplicate chart into the freshly swapped host.
  var rebuilders = [];   // anonymous, append-only
  var keyed = {};        // key -> latest fn
  var hooked = false;
  function rebuildAll() {
    setTimeout(function () {
      rebuilders.forEach(function (fn) { fn(); });
      Object.keys(keyed).forEach(function (k) { keyed[k](); });
    }, 50);
  }
  function hook() {
    if (hooked) return;
    hooked = true;
    window.addEventListener("themechange", rebuildAll);
    window.addEventListener("resize", rebuildAll);
  }
  function onRebuild(fn) { hook(); rebuilders.push(fn); }
  function rebuildKey(key, fn) { hook(); keyed[key] = fn; }

  // curve renders the shared cumulative-P&L line (dashboard hero + trades
  // performance chart): stroke green/red by final total, soft fill, x is
  // unix-seconds time. data = {xs, ys, total}; opts.moneyAxis widens Y.
  function curve(elId, data, opts) {
    opts = opts || {};
    var inst = null;
    function build() {
      if (inst) { inst.destroy(); inst = null; }
      var el = document.getElementById(elId);
      if (!el || !data.xs || !data.xs.length) return;
      var c = colors();
      var stroke = data.total >= 0 ? c.pos : c.neg;
      // The hover legend renders below the plot INSIDE el, so the plot
      // must leave it room or the legend hangs out of the card.
      inst = new uPlot({
        width: el.clientWidth,
        height: Math.max(80, el.clientHeight - 30),
        scales: { x: { time: true } },
        series: [
          {},
          {
            label: "Cumulative P&L",
            stroke: stroke,
            fill: rgba(stroke, 0.16),
            width: 1.8,
            value: function (u, v) { return v == null ? "" : v.toFixed(2); },
          },
        ],
        axes: opts.moneyAxis ? moneyAxes(c) : axes(c),
      }, [data.xs, data.ys], el);
    }
    build();
    rebuildKey("curve:" + elId, build); // keyed: safe under fragment re-runs
  }

  window.uplotHelpers = {
    colors: colors,
    rgba: rgba,
    fmtMoneyShort: fmtMoneyShort,
    fmtMoneyFull: fmtMoneyFull,
    axes: axes,
    moneyAxes: moneyAxes,
    onRebuild: onRebuild,
    rebuildKey: rebuildKey,
    curve: curve,
  };
})();
