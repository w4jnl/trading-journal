// levels.js - the day map's charts: daily, weekly and intraday candles with
// the strongest zones drawn as price lines. Reads the page's JSON island for
// the data URL, pulls the vendored lightweight-charts on first use (as
// trade-chart.js does) and renders once; the page is server-rendered.
(function () {
  "use strict";
  var island = document.getElementById("levels-page-data");
  if (!island) return;
  var page;
  try { page = JSON.parse(island.textContent); } catch (e) { return; }
  var LWC_SRC = "/static/vendor/lightweight-charts-5.2.0.standalone.production.min.js";

  function ensureLib() {
    if (window.LightweightCharts) return Promise.resolve();
    return new Promise(function (resolve, reject) {
      var s = document.createElement("script");
      s.src = LWC_SRC; s.async = true;
      s.onload = resolve; s.onerror = function () { reject(new Error("chart library failed to load")); };
      document.head.appendChild(s);
    });
  }
  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return (v || "").trim() || fallback;
  }
  function colors() {
    return { bg: cssVar("--bg-elev", "#fff"), fg: cssVar("--fg", "#18181b"), grid: cssVar("--border", "#e4e4e7"),
      pos: cssVar("--pos", "#16a34a"), neg: cssVar("--neg", "#dc2626"), muted: cssVar("--fg-muted", "#71717a"),
      accent: cssVar("--accent", "#12999D"), group: cssVar("--group", "#7c3aed"), warn: cssVar("--warn", "#d97706") };
  }
  function tz() {
    var m = document.querySelector('meta[name="trading-tz"]');
    var t = m && m.getAttribute("content");
    if (!t) return undefined;
    try { new Intl.DateTimeFormat([], { timeZone: t }); return t; } catch (e) { return undefined; }
  }

  function draw(host, bars, zones, levels, intraday) {
    var LWC = window.LightweightCharts, c = colors(), zone = tz();
    var fmt = new Intl.DateTimeFormat([], Object.assign({ month: "short", day: "numeric" }, intraday ? { hour: "2-digit", minute: "2-digit" } : {}, zone ? { timeZone: zone } : {}));
    var chart = LWC.createChart(host, {
      autoSize: true,
      layout: { background: { color: c.bg }, textColor: c.fg, fontSize: 11 },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
      rightPriceScale: { borderColor: c.grid },
      timeScale: { borderColor: c.grid, timeVisible: intraday, rightOffset: 2,
        tickMarkFormatter: function (t) { return fmt.format(t * 1000); } },
      localization: { timeFormatter: function (t) { return fmt.format(t * 1000); } },
      crosshair: { mode: LWC.CrosshairMode ? LWC.CrosshairMode.Normal : 0 },
    });
    var candle = chart.addSeries(LWC.CandlestickSeries, { upColor: c.pos, downColor: c.neg, borderVisible: false, wickUpColor: c.pos, wickDownColor: c.neg });
    candle.setData(bars.map(function (b) { return { time: b.t, open: b.o, high: b.h, low: b.l, close: b.c }; }));
    var lo = Infinity, hi = -Infinity;
    bars.slice(-80).forEach(function (b) { lo = Math.min(lo, b.l); hi = Math.max(hi, b.h); });
    var pad = (hi - lo) * 0.6;
    (zones || []).slice(0, 8).forEach(function (z) {
      if (z.center < lo - pad || z.center > hi + pad) return;
      candle.createPriceLine({ price: z.center, color: c.accent, lineWidth: 1, lineStyle: 2, axisLabelVisible: false, title: (z.kinds || []).slice(0, 2).join(",") });
    });
    (levels || []).forEach(function (l) {
      if (l.kind !== "pdh" && l.kind !== "pdl" && l.kind !== "onh" && l.kind !== "onl") return;
      if (l.price < lo - pad || l.price > hi + pad) return;
      candle.createPriceLine({ price: l.price, color: l.source === "chart" ? c.group : l.source === "gex" ? c.warn : c.muted, lineWidth: l.score >= 5 ? 2 : 1, lineStyle: 0, axisLabelVisible: false, title: l.kind });
    });
    chart.timeScale().fitContent();
  }

  // ---- The ladder: an SVG over a price window, zoomed by the wheel around the
  // cursor's price, panned by drag, reset by double-click. The server draws
  // the same ladder statically for the first paint and for pages without
  // scripts; this replaces it once the map arrives.
  var ALWAYS = { pdh: 1, pdl: 1, pdc: 1, onh: 1, onl: 1, orh: 1, orl: 1, vwap: 1, poc: 1, vah: 1, val: 1 };
  var LADDER_MAX = 22, PAD = 12;
  function tierOf(score) { return score >= 5 ? 1 : score >= 2.5 ? 2 : 3; }
  function esc(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;"); }

  function ladder(host, map) {
    var levels = (map.levels || []).filter(function (l) { return l.kind !== "round"; });
    if (!levels.length) return;
    var c = colors(), last = map.last > 0 ? map.last : levels[Math.floor(levels.length / 2)].price;
    var half = map.atr > 0 ? 1.3 * map.atr : last * 0.01;
    var fit = { lo: last - half, hi: last + half };
    var allLo = Infinity, allHi = -Infinity;
    levels.forEach(function (l) { allLo = Math.min(allLo, l.price); allHi = Math.max(allHi, l.price); });
    var allPad = (allHi - allLo) * 0.03 || 1;
    var all = { lo: Math.min(allLo - allPad, fit.lo), hi: Math.max(allHi + allPad, fit.hi) };
    var minSpan = map.atr > 0 ? 0.15 * map.atr : (fit.hi - fit.lo) * 0.05;
    var win = null; // null = the fit
    var title = document.getElementById("lv-ladder-title"), hint = document.getElementById("lv-ladder-hint");
    if (hint) hint.hidden = false;

    function render() {
      var w = win || fit, span = w.hi - w.lo;
      var H = host.clientHeight || 560, W = host.clientWidth || 400, inner = H - 2 * PAD;
      var y = function (p) { return PAD + (w.hi - p) / span * inner; };
      var inWin = levels.filter(function (l) { return l.price >= w.lo && l.price <= w.hi; });
      var pool = inWin.slice().sort(function (a, b) {
        var aa = ALWAYS[a.kind] ? 1 : 0, bb = ALWAYS[b.kind] ? 1 : 0;
        return aa !== bb ? bb - aa : b.score - a.score;
      }).slice(0, LADDER_MAX).sort(function (a, b) { return b.price - a.price; });
      var above = levels.filter(function (l) { return l.price > w.hi; }).length;
      var below = levels.filter(function (l) { return l.price < w.lo; }).length;
      var parts = [];
      var x0 = W * 0.4, x1 = W * 0.6, mid = W / 2;
      // A label is the price (about 46 px in the mono face) plus the name, cut
      // to what the side holds at about 5.6 px a character.
      var maxChars = Math.max(8, Math.floor((x0 - 8 - 46) / 5.6));
      var cut = function (name) { return name.length > maxChars ? name.slice(0, maxChars - 1) + "…" : name; };
      parts.push('<line x1="' + mid + '" y1="0" x2="' + mid + '" y2="' + H + '" stroke="' + c.grid + '"/>');
      (map.zones || []).forEach(function (z) {
        if (z.levels.length < 2 || z.high < w.lo || z.low > w.hi) return;
        var top = y(Math.min(z.high, w.hi)), bot = y(Math.max(z.low, w.lo));
        var h = Math.max(bot - top, 4);
        var kinds = (z.kinds || []).join(", ");
        parts.push('<rect x="' + W * 0.37 + '" y="' + top + '" width="' + W * 0.26 + '" height="' + h + '" rx="3" fill="' + c.accent + '" fill-opacity="0.14"><title>zone ' + z.center.toFixed(2) + ' · ' + esc(kinds) + ' · score ' + z.score + '</title></rect>');
        if (h >= 12) parts.push('<text class="band-score" x="' + (W * 0.37 + 3) + '" y="' + (top + 9) + '" fill="' + c.accent + '">' + z.score + '</text>');
      });
      var lastLabel = { l: -100, r: -100 }, GAP = 14;
      pool.forEach(function (l, i) {
        var py = y(l.price), tier = tierOf(l.score), left = i % 2 === 1;
        var col = l.source === "chart" ? c.group : l.source === "gex" ? c.warn : tier === 1 ? c.fg : tier === 2 ? c.muted : c.grid;
        var width = tier === 1 ? 2.5 : tier === 2 ? 1.5 : 1;
        var dash = tier === 3 ? ' stroke-dasharray="4 3"' : l.kind === "atr" ? ' stroke-dasharray="2 3"' : "";
        parts.push('<line x1="' + x0 + '" y1="' + py + '" x2="' + x1 + '" y2="' + py + '" stroke="' + col + '" stroke-width="' + width + '"' + dash + '><title>' + esc(l.name) + ' · ' + l.price.toFixed(2) + ' · ' + l.source + ' · score ' + l.score.toFixed(2) + '</title></line>');
        var side = left ? "l" : "r";
        var ly = Math.max(py, lastLabel[side] + GAP);
        lastLabel[side] = ly;
        var textCol = l.source === "chart" ? c.group : l.source === "gex" ? c.warn : tier === 3 ? c.muted : c.fg;
        var nameCol = tier === 1 ? textCol : c.muted;
        var cls = "t" + tier;
        if (left) {
          parts.push('<text class="' + cls + '" x="' + (x0 - 6) + '" y="' + (ly + 3.5) + '" text-anchor="end" fill="' + nameCol + '">' + esc(cut(l.name)) + ' <tspan class="p" fill="' + textCol + '">' + l.price.toFixed(2) + '</tspan><title>' + esc(l.name) + '</title></text>');
        } else {
          parts.push('<text class="' + cls + '" x="' + (x1 + 6) + '" y="' + (ly + 3.5) + '" fill="' + nameCol + '"><tspan class="p" fill="' + textCol + '">' + l.price.toFixed(2) + '</tspan> ' + esc(cut(l.name)) + '<title>' + esc(l.name) + '</title></text>');
        }
      });
      if (map.last > 0) {
        var ly2 = Math.min(H - PAD, Math.max(PAD, y(map.last)));
        parts.push('<line x1="' + W * 0.36 + '" y1="' + ly2 + '" x2="' + W * 0.64 + '" y2="' + ly2 + '" stroke="' + c.fg + '" stroke-width="2"/>');
        parts.push('<rect x="' + (mid - 30) + '" y="' + (ly2 - 8) + '" width="60" height="16" rx="3" fill="' + c.fg + '"/>');
        parts.push('<text class="p" x="' + mid + '" y="' + (ly2 + 3.5) + '" text-anchor="middle" fill="' + c.bg + '" font-weight="600">' + map.last.toFixed(2) + '</text>');
      }
      if (above) parts.push('<text x="' + mid + '" y="9" text-anchor="middle" fill="' + c.muted + '">▲ ' + above + ' more above</text>');
      if (below) parts.push('<text x="' + mid + '" y="' + (H - 2) + '" text-anchor="middle" fill="' + c.muted + '">▼ ' + below + ' more below</text>');
      host.innerHTML = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img" aria-label="the ladder">' + parts.join("") + '</svg>';
      if (title) {
        var spanAtr = map.atr > 0 ? (span / map.atr).toFixed(1) + " ATR" : span.toFixed(0) + " points";
        title.textContent = "· " + w.lo.toFixed(2) + " – " + w.hi.toFixed(2) + " (" + spanAtr + "), " + pool.length + " of " + inWin.length + " in view";
      }
    }

    host.addEventListener("wheel", function (e) {
      e.preventDefault();
      var cur = win || fit, span = cur.hi - cur.lo, allSpan = all.hi - all.lo;
      var dy = e.deltaY;
      if (e.deltaMode === 1) dy *= 33; else if (e.deltaMode === 2) dy *= 300;
      var next = Math.min(allSpan, Math.max(minSpan, span * Math.exp(dy * 0.0015)));
      var rect = host.getBoundingClientRect();
      var frac = Math.min(1, Math.max(0, (e.clientY - rect.top - PAD) / Math.max(1, rect.height - 2 * PAD)));
      var p = cur.hi - frac * span;
      var lo = p - (p - cur.lo) * (next / span), hi = lo + next;
      if (lo < all.lo) { lo = all.lo; hi = lo + next; }
      if (hi > all.hi) { hi = all.hi; lo = hi - next; }
      win = { lo: lo, hi: hi };
      render();
    }, { passive: false });
    host.addEventListener("dblclick", function () { win = null; render(); });
    var pan = null;
    host.addEventListener("pointerdown", function (e) {
      if (e.button !== 0) return;
      var cur = win || fit;
      pan = { y: e.clientY, lo: cur.lo, hi: cur.hi };
      try { host.setPointerCapture(e.pointerId); } catch (err) { /* fine */ }
    });
    host.addEventListener("pointermove", function (e) {
      if (!pan) return;
      var span = pan.hi - pan.lo, rect = host.getBoundingClientRect();
      var dp = (e.clientY - pan.y) / Math.max(1, rect.height - 2 * PAD) * span; // drag down looks higher
      var lo = pan.lo + dp, hi = pan.hi + dp;
      if (lo < all.lo) { lo = all.lo; hi = lo + span; }
      if (hi > all.hi) { hi = all.hi; lo = hi - span; }
      win = { lo: lo, hi: hi };
      render();
    });
    host.addEventListener("pointerup", function () { pan = null; });
    host.addEventListener("pointercancel", function () { pan = null; });
    if (window.__lvResize) window.removeEventListener("resize", window.__lvResize);
    window.__lvResize = render;
    window.addEventListener("resize", render);
    render();
  }

  // ---- Keep the page current: while the day is today, ask every 20 s which
  // post the map would show; a new one means a chart posted, and the live
  // region is fetched again and swapped in (the way live_refresh.js does for
  // the data version), which re-runs this script on the fresh island.
  function watchFeed() {
    if (!page.live || !page.versionURL) return;
    if (window.__lvWatch) clearInterval(window.__lvWatch);
    window.__lvWatch = setInterval(function () {
      if (document.hidden || !window.htmx) return;
      fetch(page.versionURL, { credentials: "same-origin", headers: { Accept: "application/json" } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (v) {
          if (!v || v.feedId === page.feedId) return;
          clearInterval(window.__lvWatch);
          window.__lvWatch = null;
          var url = location.pathname + location.search + (location.search ? "&" : "?") + "fragment=1";
          window.htmx.ajax("GET", url, { target: "body", swap: "none" }).catch(function () { watchFeed(); });
        })
        .catch(function () { /* next tick */ });
    }, 20000);
  }
  watchFeed();

  fetch(page.dataURL, { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (r) { if (!r.ok) throw new Error("levels data " + r.status); return r.json(); })
    .then(function (data) {
      var ladderHost = document.getElementById("lv-ladder");
      if (ladderHost && data.map) { try { ladder(ladderHost, data.map); } catch (e) { console.warn("levels ladder:", e); } }
      return ensureLib().then(function () {
        var map = data.map || {};
        document.querySelectorAll(".levels-chart").forEach(function (host) {
          var which = host.getAttribute("data-series");
          var bars = data[which] || [];
          if (!bars.length) { host.innerHTML = '<p class="muted" style="font-size:12px;">No bars.</p>'; return; }
          if (which === "daily") bars = bars.slice(-120);
          draw(host, bars, map.zones, map.levels, which === "intraday");
        });
      });
    })
    .catch(function (e) {
      document.querySelectorAll(".levels-chart").forEach(function (host) { host.innerHTML = '<p class="muted" style="font-size:12px;">Chart unavailable.</p>'; });
      console.warn("levels:", e);
    });
})();
