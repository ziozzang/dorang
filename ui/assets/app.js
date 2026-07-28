// dorang admin UI — the whole client-side program.
//
// Everything that can be rendered on the server is rendered on the server. What
// is left here is genuinely client-side and nothing else:
//
//   1. the theme toggle, because the operator's preference is not the server's
//      business and a round trip to change a colour is absurd;
//   2. a substring filter over an already-loaded table, because filtering rows
//      the browser already has must not cost a query;
//   3. an optional refresh timer, which reloads the current URL and therefore
//      re-runs the same bounded, paginated query the server already validated.
//
// There is no framework, no bundler and no network fetch. The screens are
// read-only (see internal/admin/ui.go for why: a cookie-authenticated mutating
// surface is a CSRF surface, and this tool does not need one).

(function () {
  "use strict";

  // ---- theme ------------------------------------------------------------
  //
  // No stored preference means "follow the system", which is the CSS default;
  // an explicit choice is stamped on <html> where the data-theme rules win.

  var KEY = "dorang.theme";

  function stored() {
    try {
      return window.localStorage.getItem(KEY);
    } catch (e) {
      return null; // private mode, or storage disabled. Not an error here.
    }
  }

  function store(v) {
    try {
      if (v === null) {
        window.localStorage.removeItem(KEY);
      } else {
        window.localStorage.setItem(KEY, v);
      }
    } catch (e) {
      /* ignore: the toggle still works for this page load */
    }
  }

  function applyTheme(v) {
    var root = document.documentElement;
    if (v === "light" || v === "dark") {
      root.setAttribute("data-theme", v);
    } else {
      root.removeAttribute("data-theme");
    }
  }

  function systemTheme() {
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  }

  function cycleTheme() {
    var cur = stored();
    var next;
    if (cur === null) {
      // First press flips away from whatever the system is showing, which is
      // what the operator is looking at and therefore what they meant.
      next = systemTheme() === "dark" ? "light" : "dark";
    } else if (cur === "dark") {
      next = "light";
    } else {
      next = null; // back to following the system
    }
    store(next);
    applyTheme(next);
    label(next);
  }

  function label(v) {
    var b = document.getElementById("theme-toggle");
    if (!b) return;
    b.textContent = v === null ? "theme: auto" : "theme: " + v;
    b.setAttribute("aria-label", "Colour theme: " + (v === null ? "follow system" : v));
  }

  // ---- table filter -----------------------------------------------------

  function wireFilter() {
    var input = document.getElementById("filter");
    if (!input) return;
    var count = document.getElementById("filter-count");
    input.addEventListener("input", function () {
      var q = input.value.trim().toLowerCase();
      var shown = 0;
      var total = 0;
      var bodies = document.querySelectorAll("table[data-filterable] tbody");
      for (var b = 0; b < bodies.length; b++) {
        var rows = bodies[b].rows;
        for (var i = 0; i < rows.length; i++) {
          total++;
          var hit = q === "" || rows[i].textContent.toLowerCase().indexOf(q) !== -1;
          rows[i].hidden = !hit;
          if (hit) shown++;
        }
      }
      if (count) {
        count.textContent = q === "" ? total + " rows" : shown + " of " + total + " rows";
      }
    });
  }

  // ---- refresh ----------------------------------------------------------

  function wireRefresh() {
    var sel = document.getElementById("refresh");
    if (!sel) return;
    var timer = null;
    function arm() {
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
      var secs = parseInt(sel.value, 10);
      if (!isNaN(secs) && secs > 0) {
        timer = window.setTimeout(function () {
          window.location.reload();
        }, secs * 1000);
      }
    }
    sel.addEventListener("change", arm);
    arm();
  }

  // ---- boot -------------------------------------------------------------

  applyTheme(stored());

  document.addEventListener("DOMContentLoaded", function () {
    label(stored());
    var b = document.getElementById("theme-toggle");
    if (b) b.addEventListener("click", cycleTheme);
    wireFilter();
    wireRefresh();
  });
})();
