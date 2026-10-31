// rcfg-sim-ui Activity screen: heatmap, live charts and event tail.
//
// The page has one server-sent event stream, opened by htmx-ext-sse
// (sse-connect="/activity/stream"), which swaps the summary strip in as
// HTML. This script reads the JSON messages (history, rates, frame, event,
// dropped) from that same EventSource, by wrapping htmx.createEventSource,
// so a tab holds one connection and closing it ends everything.
// No inline script or eval: the page's Content-Security-Policy forbids both.
(function () {
  "use strict";
  if (!window.htmx || !window.uPlot) return;

  var STREAM = "/activity/stream";
  var WINDOW = 600; // seconds of chart history

  var origCreate = htmx.createEventSource;
  htmx.createEventSource = function (url) {
    var es = origCreate(url);
    if (String(url).indexOf(STREAM) === 0) attach(es);
    return es;
  };

  function attach(es) {
    on(es, "history", function (d) { charts.load(d || []); });
    on(es, "rates", function (d) { charts.push(d); });
    on(es, "frame", function (d) { heat.setFrame(d); });
    on(es, "event", function (d) { tail.add(d); });
    on(es, "dropped", function (d) { tail.dropped(d); });
  }

  function on(es, name, fn) {
    es.addEventListener(name, function (e) {
      var d;
      try { d = JSON.parse(e.data); } catch (err) { return; }
      fn(d);
    });
  }

  // --- Colours: the theme's CSS variables, resolved to RGB. ---

  var probe = document.createElement("canvas");
  probe.width = probe.height = 1;
  var probeCtx = probe.getContext("2d", { willReadFrequently: true });

  function cssColor(varName) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(varName).trim();
    probeCtx.clearRect(0, 0, 1, 1);
    probeCtx.fillStyle = "#000";
    probeCtx.fillStyle = v || "#000";
    probeCtx.fillRect(0, 0, 1, 1);
    var p = probeCtx.getImageData(0, 0, 1, 1).data;
    return [p[0], p[1], p[2]];
  }

  function rgb(c, a) {
    return a == null ? "rgb(" + c.join(",") + ")" : "rgba(" + c.join(",") + "," + a + ")";
  }

  // State code -> colour variable (section 6.1, plus 255 for unreachable).
  var STATE_VARS = {
    0: "--ui-st-never", 1: "--ui-st-ok", 2: "--ui-st-auth", 3: "--ui-st-disconnect",
    4: "--ui-st-error", 5: "--ui-st-hang", 6: "--ui-st-inactive", 7: "--ui-st-active", 255: "--ui-panel-2"
  };
  var STATE_NAMES = {
    0: "never", 1: "ok", 2: "auth_fail", 3: "disconnect", 4: "error", 5: "hang", 6: "inactive", 7: "active",
    255: "unreachable"
  };

  // --- Heatmap ---

  var heat = {
    canvas: document.getElementById("heatmap"),
    wrap: document.getElementById("heat-wrap"),
    tip: document.getElementById("heat-tip"),
    stats: document.getElementById("heat-stats"),
    off: document.createElement("canvas"),
    frame: null,
    states: null,
    seen: null,
    cols: 1,
    rows: 1,
    cell: 4,
    palette: {},
    hosts: {},
    hostsKey: "",
    pending: false,

    setFrame: function (f) {
      this.frame = f;
      this.states = b64(f.state);
      var ls = b64(f.last_seen);
      this.seen = new DataView(ls.buffer, ls.byteOffset, ls.byteLength);
      var key = (f.instances || []).map(function (i) { return i.name + ":" + i.count; }).join(",");
      if (key !== this.hostsKey) {
        this.hostsKey = key;
        loadHosts();
      }
      if (!this.pending) {
        this.pending = true;
        requestAnimationFrame(function () { heat.pending = false; heat.draw(); });
      }
    },

    refreshPalette: function () {
      var p = {};
      Object.keys(STATE_VARS).forEach(function (k) { p[k] = cssColor(STATE_VARS[k]); });
      this.palette = p;
    },

    draw: function () {
      var f = this.frame;
      if (!f || !this.canvas) return;
      var t0 = performance.now();
      var n = f.total;
      var cols = Math.min(250, Math.max(1, Math.ceil(Math.sqrt(n * 1.25))));
      var rows = Math.max(1, Math.ceil(n / cols));
      var off = this.off, octx = off.getContext("2d");
      if (off.width !== cols || off.height !== rows) {
        off.width = cols;
        off.height = rows;
        this.img = octx.createImageData(cols, rows);
      }
      var data = this.img.data, st = this.states, pal = this.palette, fallback = pal[0] || [0, 0, 0];
      data.fill(0);
      for (var i = 0; i < n; i++) {
        var c = pal[st[i]] || fallback, j = i * 4;
        data[j] = c[0]; data[j + 1] = c[1]; data[j + 2] = c[2]; data[j + 3] = 255;
      }
      octx.putImageData(this.img, 0, 0);

      var cell = Math.max(2, Math.min(24, Math.floor((this.wrap.clientWidth || 250) / cols)));
      var w = cols * cell, h = rows * cell;
      if (this.canvas.width !== w || this.canvas.height !== h) {
        this.canvas.width = w;
        this.canvas.height = h;
      }
      var ctx = this.canvas.getContext("2d");
      ctx.imageSmoothingEnabled = false;
      ctx.clearRect(0, 0, w, h);
      ctx.drawImage(off, 0, 0, w, h);
      this.cols = cols; this.rows = rows; this.cell = cell;
      if (this.stats) {
        this.stats.textContent = n.toLocaleString() + " cells · drawn in " + (performance.now() - t0).toFixed(1) + " ms";
      }
    },

    // cellAt maps a mouse event to {index, instance, local}.
    cellAt: function (e) {
      var f = this.frame;
      if (!f) return null;
      var r = this.canvas.getBoundingClientRect();
      var sx = this.canvas.width / r.width, sy = this.canvas.height / r.height;
      var col = Math.floor((e.clientX - r.left) * sx / this.cell);
      var row = Math.floor((e.clientY - r.top) * sy / this.cell);
      if (col < 0 || col >= this.cols || row < 0) return null;
      var i = row * this.cols + col;
      if (i >= f.total) return null;
      var ins = f.instances || [];
      for (var k = 0; k < ins.length; k++) {
        if (i >= ins[k].offset && i < ins[k].offset + ins[k].count) {
          return { index: i, instance: ins[k], local: i - ins[k].offset };
        }
      }
      return null;
    },

    describe: function (c) {
      var f = this.frame;
      var host = (this.hosts[c.instance.name] || [])[c.local] || "port " + (c.instance.port_start + c.local);
      var state = STATE_NAMES[this.states[c.index]] || "never";
      var seen = "never";
      if (!c.instance.reachable) {
        seen = "instance not reporting";
      } else {
        var ts = this.seen.getUint32(c.index * 4, true);
        if (ts) seen = ago(f.now - ts) + " ago";
      }
      return host + " · " + state + " · last seen " + seen;
    }
  };

  function ago(s) {
    s = Math.max(0, s);
    if (s < 60) return s + "s";
    if (s < 3600) return Math.floor(s / 60) + "m";
    return Math.floor(s / 3600) + "h";
  }

  function b64(s) {
    var bin = atob(s || "");
    var a = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) a[i] = bin.charCodeAt(i);
    return a;
  }

  function loadHosts() {
    fetch("/activity/hosts", { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : {}; })
      .then(function (h) { heat.hosts = h || {}; })
      .catch(function () {});
  }

  if (heat.canvas) {
    heat.canvas.addEventListener("mousemove", function (e) {
      var c = heat.cellAt(e);
      if (!c) { heat.tip.classList.add("hidden"); return; }
      heat.tip.textContent = heat.describe(c);
      heat.tip.classList.remove("hidden");
      var wr = heat.wrap.getBoundingClientRect();
      var x = e.clientX - wr.left + 12, y = e.clientY - wr.top + 12;
      if (x + heat.tip.offsetWidth > wr.width) x = Math.max(0, e.clientX - wr.left - heat.tip.offsetWidth - 12);
      heat.tip.style.left = x + "px";
      heat.tip.style.top = y + "px";
    });
    heat.canvas.addEventListener("mouseleave", function () { heat.tip.classList.add("hidden"); });
    heat.canvas.addEventListener("click", function (e) {
      var c = heat.cellAt(e);
      if (!c) return;
      htmx.ajax("GET", "/activity/device?instance=" + encodeURIComponent(c.instance.name) + "&index=" + c.local,
        { target: "#device-panel", swap: "innerHTML" });
    });
  }

  // --- Charts (uPlot) ---

  var SPECS = [
    { id: "chart-active", series: [{ label: "active", key: function (p) { return p.active; }, color: "--ui-st-active" }] },
    { id: "chart-sessions", series: [
      { label: "ok", key: function (p) { return p.sessions.ok; }, color: "--ui-st-ok" },
      { label: "error", key: function (p) { return p.sessions.error; }, color: "--ui-st-error" },
      { label: "auth_fail", key: function (p) { return p.sessions.auth_fail; }, color: "--ui-st-auth" },
      { label: "disconnect", key: function (p) { return p.sessions.disconnect; }, color: "--ui-st-disconnect" }
    ] },
    { id: "chart-bytes", series: [{ label: "bytes/s", key: function (p) { return p.bytes; }, color: "--color-accent-500" }] },
    { id: "chart-faults", series: [
      { label: "auth_fail", key: function (p) { return p.faults.auth_fail; }, color: "--ui-st-auth" },
      { label: "disconnect_mid", key: function (p) { return p.faults.disconnect_mid; }, color: "--ui-st-disconnect" },
      { label: "slow_response", key: function (p) { return p.faults.slow_response; }, color: "--ui-st-hang" },
      { label: "malformed", key: function (p) { return p.faults.malformed; }, color: "--ui-st-error" }
    ] }
  ];

  var charts = {
    points: [],
    plots: [],

    load: function (pts) {
      this.points = pts.slice();
      this.trim();
      this.build();
    },

    push: function (p) {
      var last = this.points[this.points.length - 1];
      if (last && last.t >= p.t) return;
      this.points.push(p);
      this.trim();
      this.update();
    },

    trim: function () {
      var last = this.points[this.points.length - 1];
      if (!last) return;
      var cut = last.t - WINDOW, i = 0;
      while (i < this.points.length && this.points[i].t < cut) i++;
      if (i) this.points = this.points.slice(i);
    },

    data: function (spec) {
      var xs = this.points.map(function (p) { return p.t; });
      var out = [xs];
      spec.series.forEach(function (s) {
        out.push(charts.points.map(function (p) { var v = s.key(p); return v == null ? 0 : v; }));
      });
      return out;
    },

    build: function () {
      this.plots.forEach(function (p) { p.plot.destroy(); });
      this.plots = [];
      var fg = rgb(cssColor("--ui-muted")), grid = rgb(cssColor("--ui-line"));
      SPECS.forEach(function (spec) {
        var el = document.getElementById(spec.id);
        if (!el) return;
        var series = [{}];
        spec.series.forEach(function (s) {
          var c = cssColor(s.color);
          series.push({ label: s.label, stroke: rgb(c), fill: rgb(c, 0.08), width: 1.5, points: { show: false } });
        });
        var axis = { stroke: fg, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
        var plot = new uPlot({
          width: Math.max(200, el.clientWidth),
          height: 190,
          series: series,
          axes: [axis, Object.assign({ size: 60 }, axis)],
          scales: { x: { time: true } },
          legend: { live: true },
          cursor: { drag: { x: false, y: false } }
        }, charts.data(spec), el);
        charts.plots.push({ spec: spec, plot: plot, el: el });
      });
    },

    update: function () {
      this.plots.forEach(function (p) { p.plot.setData(charts.data(p.spec)); });
    },

    resize: function () {
      this.plots.forEach(function (p) { p.plot.setSize({ width: Math.max(200, p.el.clientWidth), height: 190 }); });
    }
  };

  // --- Event tail ---

  var tail = {
    body: document.getElementById("tail-body"),
    status: document.getElementById("tail-status"),
    droppedEl: document.getElementById("tail-dropped"),
    pauseBtn: document.getElementById("tail-pause"),
    host: document.getElementById("tail-host"),
    outcome: document.getElementById("tail-outcome"),
    fault: document.getElementById("tail-fault"),
    events: [],
    max: 500,
    paused: false,
    missed: 0,
    seen: 0,
    lost: 0,
    lostUpstream: 0,
    pending: false,

    add: function (ev) {
      this.seen++;
      this.events.push(ev);
      if (this.events.length > this.max) this.events.shift();
      if (this.paused) { this.missed++; this.updateStatus(); return; }
      this.schedule();
    },

    dropped: function (d) {
      this.lost += d.n || 0;
      this.lostUpstream += d.upstream || 0;
      if (this.droppedEl) {
        this.droppedEl.textContent = "Dropped " + this.lost + " here, " + this.lostUpstream + " at the instances (rate caps).";
      }
    },

    schedule: function () {
      if (this.pending) return;
      this.pending = true;
      requestAnimationFrame(function () { tail.pending = false; tail.render(); });
    },

    match: function (ev) {
      var h = this.host.value.trim().toLowerCase();
      if (h && String(ev.host || "").toLowerCase().indexOf(h) < 0) return false;
      if (this.outcome.value && ev.outcome !== this.outcome.value) return false;
      if (this.fault.value && ev.fault !== this.fault.value) return false;
      return true;
    },

    render: function () {
      if (!this.body) return;
      var frag = document.createDocumentFragment(), shown = 0;
      for (var i = this.events.length - 1; i >= 0 && shown < 200; i--) {
        var ev = this.events[i];
        if (!this.match(ev)) continue;
        shown++;
        var tr = document.createElement("tr");
        tr.className = "border-t border-line";
        [time(ev.t), ev.kind, ev.host, ev.port, ev.outcome || "", ev.fault || "", ev.bytes, ev.ms].forEach(function (v, k) {
          var td = document.createElement("td");
          td.className = "px-2 py-0.5" + (k >= 6 ? " text-right" : "");
          td.textContent = v == null ? "" : String(v);
          tr.appendChild(td);
        });
        frag.appendChild(tr);
      }
      this.body.replaceChildren(frag);
      this.updateStatus(shown);
    },

    updateStatus: function (shown) {
      if (!this.status) return;
      if (this.paused) {
        this.status.textContent = "Paused. " + this.missed + " new events since.";
      } else {
        this.status.textContent = this.seen + " events received" + (shown != null ? ", " + shown + " shown" : "") + ".";
      }
    },

    togglePause: function () {
      this.paused = !this.paused;
      this.pauseBtn.setAttribute("aria-pressed", String(this.paused));
      this.pauseBtn.textContent = this.paused ? "Resume" : "Pause";
      if (!this.paused) { this.missed = 0; this.render(); } else { this.updateStatus(); }
    }
  };

  function time(ms) {
    var d = new Date(ms);
    return d.toTimeString().slice(0, 8) + "." + String(d.getMilliseconds()).padStart(3, "0");
  }

  if (tail.pauseBtn) tail.pauseBtn.addEventListener("click", function () { tail.togglePause(); });
  [tail.host, tail.outcome, tail.fault].forEach(function (el) {
    if (el) el.addEventListener("input", function () { tail.render(); });
  });

  // --- Theme and size changes ---

  function retheme() {
    heat.refreshPalette();
    heat.draw();
    if (charts.plots.length) charts.build();
  }
  new MutationObserver(retheme).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: light)");
    if (mq.addEventListener) mq.addEventListener("change", retheme);
  }
  var resizeTimer = 0;
  window.addEventListener("resize", function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () { heat.draw(); charts.resize(); }, 150);
  });

  heat.refreshPalette();
})();
