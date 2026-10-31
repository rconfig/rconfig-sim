// rcfg-sim-ui behaviour that htmx does not cover. No inline scripts or
// eval: the page's Content-Security-Policy forbids both.
(function () {
  "use strict";

  // Theme toggle: buttons with data-theme-set.
  function markTheme() {
    var cur = document.documentElement.getAttribute("data-theme");
    document.querySelectorAll("[data-theme-set]").forEach(function (b) {
      b.setAttribute("aria-pressed", String(b.getAttribute("data-theme-set") === cur));
    });
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-theme-set]");
    if (!b) return;
    var t = b.getAttribute("data-theme-set");
    document.documentElement.setAttribute("data-theme", t);
    try { localStorage.setItem("rcfg-sim-ui-theme", t); } catch (err) {}
    markTheme();
  });

  // Keep a slider and its number input in step: <input type=range
  // data-sync="id-of-number-input">. The number input carries the name.
  document.addEventListener("input", function (e) {
    var el = e.target;
    var id = el.getAttribute && el.getAttribute("data-sync");
    if (id) {
      var other = document.getElementById(id);
      if (other) { other.value = el.value; other.dispatchEvent(new Event("input", { bubbles: true })); }
      return;
    }
    if (el.id) {
      document.querySelectorAll('[data-sync="' + el.id + '"]').forEach(function (r) { r.value = el.value; });
    }
    updateEstimates();
  });

  // "Allow up to 100%" toggles: <input type=checkbox data-extend="range-id,...">.
  document.addEventListener("change", function (e) {
    var ids = e.target.getAttribute && e.target.getAttribute("data-extend");
    if (!ids) return;
    ids.split(",").forEach(function (id) {
      var r = document.getElementById(id);
      if (r) r.max = e.target.checked ? "100" : r.getAttribute("data-default-max");
    });
  });

  // Live fault estimates: <span data-estimate="number-input-id" data-devices="N">.
  function updateEstimates() {
    document.querySelectorAll("[data-estimate]").forEach(function (s) {
      var inp = document.getElementById(s.getAttribute("data-estimate"));
      var n = Number(s.getAttribute("data-devices")) || 0;
      var pct = inp ? Number(inp.value) : 0;
      if (!isFinite(pct) || pct < 0) pct = 0;
      s.textContent = String(Math.round(n * pct / 100));
    });
  }

  // Remove a profile rule row: <button data-remove-row>.
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-remove-row]");
    if (!b) return;
    var row = b.closest("[data-row]");
    if (!row) return;
    var form = row.closest("form");
    row.remove();
    if (form) form.dispatchEvent(new Event("input", { bubbles: true }));
  });

  // Copy to clipboard: <button data-copy="element-id">.
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-copy]");
    if (!b) return;
    var src = document.getElementById(b.getAttribute("data-copy"));
    if (!src || !navigator.clipboard) return;
    navigator.clipboard.writeText(src.textContent.trim()).then(function () {
      var old = b.textContent;
      b.textContent = "Copied";
      setTimeout(function () { b.textContent = old; }, 1200);
    });
  });

  document.addEventListener("DOMContentLoaded", function () { markTheme(); updateEstimates(); });
  document.addEventListener("htmx:afterSettle", updateEstimates);
})();
