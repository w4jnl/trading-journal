// trade-chart.js - trade-replay candlestick charts.
//
// Mount points are <div class="trade-chart" data-chart-endpoint="/charts/..."
// data-chart-cached="true|false">. On a cache hit we auto-fetch the JSON and
// render; on a miss we show a "Load chart" button that fetches with ?load=1
// (the only path that hits the upstream data source). The component lazily
// pulls TradingView lightweight-charts v5 from a CDN on first use, so pages
// without charts pay nothing, and re-initialises after HTMX swaps (the trade
// detail modal) the same way trade_sheet.js does.

(function () {
  "use strict";

  var LWC_SRC = "https://unpkg.com/lightweight-charts@5/dist/lightweight-charts.standalone.production.js";
  var libPromise = null;

  function ensureLib() {
    if (window.LightweightCharts) return Promise.resolve();
    if (libPromise) return libPromise;
    libPromise = new Promise(function (resolve, reject) {
      var s = document.createElement("script");
      s.src = LWC_SRC;
      s.async = true;
      s.onload = function () { resolve(); };
      s.onerror = function () { reject(new Error("chart library failed to load")); };
      document.head.appendChild(s);
    });
    return libPromise;
  }

  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    v = (v || "").trim();
    return v || fallback;
  }

  function themeColors() {
    return {
      bg: cssVar("--bg-elev", "#ffffff"),
      fg: cssVar("--fg", "#18181b"),
      grid: cssVar("--border", "#e4e4e7"),
      pos: cssVar("--pos", "#16a34a"),
      neg: cssVar("--neg", "#dc2626"),
      muted: cssVar("--fg-muted", "#71717a"),
    };
  }

  function injectStyles() {
    if (document.getElementById("trade-chart-styles")) return;
    var s = document.createElement("style");
    s.id = "trade-chart-styles";
    s.textContent =
      ".trade-chart{display:block;margin:12px 0}" +
      ".trade-chart__panel{margin-bottom:14px;border:1px solid var(--border);border-radius:8px;overflow:hidden;background:var(--bg-elev)}" +
      ".trade-chart__head{display:flex;justify-content:space-between;font-size:12px;font-weight:600;color:var(--fg-muted);padding:6px 10px;border-bottom:1px solid var(--border)}" +
      ".trade-chart__canvas{height:340px;width:100%}" +
      ".trade-chart__note{font-size:11px;color:var(--fg-muted);padding:4px 10px}" +
      ".trade-chart__msg{font-size:13px;color:var(--fg-muted);padding:18px 10px;text-align:center}" +
      ".trade-chart__prompt{padding:14px;text-align:center}";
    document.head.appendChild(s);
  }

  // ---------- mount lifecycle ----------

  function scan(root) {
    if (!root || !root.querySelectorAll) return;
    if (root.classList && root.classList.contains("trade-chart")) initMount(root);
    root.querySelectorAll(".trade-chart").forEach(initMount);
  }

  function initMount(el) {
    if (el.dataset.tcInit === "1") return;
    el.dataset.tcInit = "1";
    var endpoint = el.getAttribute("data-chart-endpoint");
    if (!endpoint) return;
    injectStyles();
    if (el.getAttribute("data-chart-cached") === "true") {
      loadAndRender(el, endpoint);
    } else {
      renderLoadButton(el, endpoint);
    }
  }

  function renderLoadButton(el, endpoint) {
    clearCharts(el);
    el.innerHTML = "";
    var wrap = document.createElement("div");
    wrap.className = "trade-chart__prompt";
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn--ghost btn--sm";
    btn.textContent = "Load chart";
    btn.addEventListener("click", function () {
      btn.disabled = true;
      btn.textContent = "Loading…";
      var url = endpoint + (endpoint.indexOf("?") >= 0 ? "&" : "?") + "load=1";
      loadAndRender(el, url);
    });
    wrap.appendChild(btn);
    el.appendChild(wrap);
  }

  function loadAndRender(el, url) {
    fetch(url, { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("http " + r.status);
        return r.json();
      })
      .then(function (env) {
        return ensureLib().then(function () { renderEnvelope(el, env); });
      })
      .catch(function (e) {
        console.error("trade-chart", e);
        renderMessage(el, "Chart unavailable.");
      });
  }

  function renderMessage(el, msg) {
    clearCharts(el);
    el.innerHTML = "";
    var p = document.createElement("div");
    p.className = "trade-chart__msg";
    p.textContent = msg;
    el.appendChild(p);
  }

  function clearCharts(el) {
    if (el.__tcCharts) {
      el.__tcCharts.forEach(function (c) { try { c.remove(); } catch (_) {} });
    }
    el.__tcCharts = [];
  }

  function renderEnvelope(el, env) {
    clearCharts(el);
    el.innerHTML = "";
    el.__tcEnv = env; // retained so a theme change can rebuild from the same data
    if (!env || !env.panels || env.panels.length === 0) {
      renderMessage(el, "No market data.");
      return;
    }
    env.panels.forEach(function (panel) {
      var card = document.createElement("div");
      card.className = "trade-chart__panel";

      var head = document.createElement("div");
      head.className = "trade-chart__head";
      var left = document.createElement("span");
      left.textContent = panel.contract || panel.symbolRoot || "";
      var right = document.createElement("span");
      right.textContent = panel.timeframe ? panel.timeframe + " bars" : "";
      head.appendChild(left);
      head.appendChild(right);
      card.appendChild(head);

      var holder = document.createElement("div");
      holder.className = "trade-chart__canvas";
      card.appendChild(holder);

      if (panel.note) {
        var note = document.createElement("div");
        note.className = "trade-chart__note";
        note.textContent = panel.note;
        card.appendChild(note);
      }
      el.appendChild(card);

      if (panel.bars && panel.bars.length) {
        var chart = drawPanel(holder, panel);
        if (chart) el.__tcCharts.push(chart);
      } else {
        renderMessage(holder, "No market data for this period.");
      }
    });
  }

  // ---------- rendering ----------

  function drawPanel(holder, panel) {
    var LWC = window.LightweightCharts;
    if (!LWC) return null;
    var c = themeColors();
    var chart = LWC.createChart(holder, {
      autoSize: true,
      layout: { background: { color: c.bg }, textColor: c.fg, attributionLogo: false },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
      rightPriceScale: { borderColor: c.grid },
      timeScale: {
        borderColor: c.grid,
        timeVisible: true,
        secondsVisible: panel.timeframe === "1m",
        rightOffset: 3,
      },
      crosshair: { mode: LWC.CrosshairMode ? LWC.CrosshairMode.Normal : 0 },
    });

    var candle = chart.addSeries(LWC.CandlestickSeries, {
      upColor: c.pos,
      downColor: c.neg,
      borderVisible: false,
      wickUpColor: c.pos,
      wickDownColor: c.neg,
    });
    candle.setData(panel.bars.map(function (b) {
      return { time: b.t, open: b.o, high: b.h, low: b.l, close: b.c };
    }));

    // Volume as an overlay histogram pinned to the bottom ~18% of the pane.
    var vol = chart.addSeries(LWC.HistogramSeries, { priceFormat: { type: "volume" }, priceScaleId: "" });
    chart.priceScale("").applyOptions({ scaleMargins: { top: 0.82, bottom: 0 } });
    vol.setData(panel.bars.map(function (b) {
      return { time: b.t, value: b.v || 0, color: (b.c >= b.o ? c.pos : c.neg) + "55" };
    }));

    // Entry/exit arrows, each snapped to its containing bar so the library
    // always finds a matching data point.
    var times = panel.bars.map(function (b) { return b.t; });
    var markers = (panel.markers || []).map(function (m) {
      return {
        time: snap(times, m.t),
        position: markerPosition(panel.direction, m.role),
        color: m.side === "buy" ? c.pos : c.neg,
        shape: m.side === "buy" ? "arrowUp" : "arrowDown",
        text: fmtPrice(m.price),
      };
    }).sort(function (a, b) { return a.time - b.time; });
    if (markers.length) {
      if (LWC.createSeriesMarkers) {
        LWC.createSeriesMarkers(candle, markers);
      } else if (candle.setMarkers) {
        candle.setMarkers(markers); // v4 fallback
      }
    }

    var dashed = LWC.LineStyle ? LWC.LineStyle.Dashed : 2;
    if (panel.entryAvg) {
      candle.createPriceLine({ price: panel.entryAvg, color: c.muted, lineWidth: 1, lineStyle: dashed, axisLabelVisible: true, title: "in" });
    }
    if (panel.exitAvg) {
      candle.createPriceLine({ price: panel.exitAvg, color: c.muted, lineWidth: 1, lineStyle: dashed, axisLabelVisible: true, title: "out" });
    }

    chart.timeScale().fitContent();
    return chart;
  }

  function markerPosition(direction, role) {
    // A long is entered low (arrow below the bar) and exited high (above);
    // a short is the mirror image.
    var entryBelow = direction !== "short";
    if (role === "entry") return entryBelow ? "belowBar" : "aboveBar";
    return entryBelow ? "aboveBar" : "belowBar";
  }

  function fmtPrice(p) {
    return (Math.round(p * 100) / 100).toString();
  }

  // snap returns the largest bar time <= t (the bar that contains the fill),
  // clamped to the data range.
  function snap(times, t) {
    if (!times.length) return t;
    if (t <= times[0]) return times[0];
    if (t >= times[times.length - 1]) return times[times.length - 1];
    var lo = 0, hi = times.length - 1, ans = times[0];
    while (lo <= hi) {
      var mid = (lo + hi) >> 1;
      if (times[mid] <= t) { ans = times[mid]; lo = mid + 1; } else { hi = mid - 1; }
    }
    return ans;
  }

  // ---------- init + theme ----------

  function ready(fn) {
    if (document.readyState !== "loading") fn();
    else document.addEventListener("DOMContentLoaded", fn);
  }

  ready(function () { scan(document); });

  document.addEventListener("htmx:afterSwap", function (e) { scan(e.target); });

  window.addEventListener("themechange", function () {
    document.querySelectorAll(".trade-chart").forEach(function (el) {
      if (el.__tcEnv) renderEnvelope(el, el.__tcEnv);
    });
  });
})();
