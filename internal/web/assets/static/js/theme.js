// Runs synchronously from <head>, before the first paint, so a saved theme
// never flashes the wrong colours. "system" (or no saved choice) leaves
// data-theme off and lets the prefers-color-scheme media query decide.
(function () {
  var KEY = "bitnews-theme";
  var root = document.documentElement;

  function saved() {
    try {
      var v = window.localStorage.getItem(KEY);
      return v === "light" || v === "dark" ? v : "system";
    } catch (e) {
      return "system";
    }
  }

  function apply(mode) {
    if (mode === "light" || mode === "dark") {
      root.setAttribute("data-theme", mode);
    } else {
      root.removeAttribute("data-theme");
    }
  }

  window.bitnewsTheme = {
    get: saved,
    set: function (mode) {
      try {
        if (mode === "light" || mode === "dark") {
          window.localStorage.setItem(KEY, mode);
        } else {
          window.localStorage.removeItem(KEY);
        }
      } catch (e) {
        // Storage can be blocked; the choice then lasts for this page only.
      }
      apply(mode);
    },
  };

  apply(saved());
})();
