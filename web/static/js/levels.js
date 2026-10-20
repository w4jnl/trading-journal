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

  fetch(page.dataURL, { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (r) { if (!r.ok) throw new Error("levels data " + r.status); return r.json(); })
    .then(function (data) {
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
