// Sets the theme before first paint: dark (default), light or auto, from
// localStorage. Loaded synchronously in <head>; app.js handles the toggle.
(function () {
  var t = "dark";
  try { t = localStorage.getItem("rcfg-sim-ui-theme") || "dark"; } catch (e) {}
  if (t !== "light" && t !== "auto") t = "dark";
  document.documentElement.setAttribute("data-theme", t);
})();
