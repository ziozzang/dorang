// Session forms, theme, table filters and clipboard interactions.

(function () {
  "use strict";

  // ---- theme ------------------------------------------------------------
  //
  // No stored preference means "follow the system", which is the CSS default;
  // an explicit choice is stamped on <html> where the data-theme rules win.

  var KEY = "dorang.theme";
  function t(key) { return window.dorangI18n ? window.dorangI18n.t(key) : key; }

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
    // The button is icon-only (the glyph is a CSS ::before that tracks
    // data-theme), so the label is carried by title/aria-label rather than
    // textContent — writing text here would sit beside the icon.
    var caption = t("Colour theme") + ": " + t(v === null ? "follow system" : v);
    b.setAttribute("aria-label", caption);
    b.setAttribute("title", caption);
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
        count.textContent = q === "" ? total + " " + t("rows") : shown + " " + t("of") + " " + total + " " + t("rows");
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

  // ---- the one-time secret ----------------------------------------------
  //
  // Two behaviours, both about the same 20 seconds: the field selects itself
  // when focused, and the button copies it. If the clipboard API is
  // unavailable — an insecure origin, or a browser that refuses — the field is
  // selected instead and the caption says to press the copy key, because
  // silently doing nothing is how an operator navigates away believing they
  // have the key.

  function wireSecret() {
    var field = document.getElementById("secret-value");
    if (!field) return;
    field.addEventListener("focus", function () {
      field.select();
    });

    var btn = document.getElementById("copy-secret");
    var state = document.getElementById("copy-state");
    if (!btn) return;

    function say(msg) {
      if (state) state.textContent = t(msg);
    }

    btn.addEventListener("click", function () {
      field.focus();
      field.select();
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(field.value).then(
          function () {
            say("copied — now paste it into your secret manager and close this tab");
          },
          function () {
            say("this browser would not let the page write to the clipboard; the field is selected, copy it yourself");
          }
        );
        return;
      }
      say("the field is selected; copy it with your keyboard");
    });
  }

  // ---- boot -------------------------------------------------------------

  applyTheme(stored());
  document.addEventListener("dorang:language", function () { label(stored()); var filter = document.getElementById("filter"); if (filter) filter.dispatchEvent(new Event("input")); });

  document.addEventListener("DOMContentLoaded", function () {
    label(stored());
    var b = document.getElementById("theme-toggle");
    if (b) b.addEventListener("click", cycleTheme);
    wireFilter();
    wireRefresh();
    wireSecret();
    startLiveRefresh();
  });

  // ---- live refresh -----------------------------------------------------
  // A screen may mark one region live: <div data-live="10"> re-fetches the
  // current URL every 10s and swaps just that region's innerHTML, so the
  // numbers move without a full navigation or a lost scroll position. Fetch
  // is same-origin (the CSP allows connect-src 'self' and nothing else).
  function startLiveRefresh() {
    var region = document.querySelector("[data-live]");
    if (!region) return;
    var secs = parseInt(region.getAttribute("data-live"), 10);
    if (!secs || secs < 2) secs = 10;
    var busy = false;
    setInterval(function () {
      if (busy || document.hidden) return;
      busy = true;
      region.classList.add("stale");
      fetch(window.location.href, { credentials: "same-origin", headers: { "X-Requested-With": "fetch" } })
        .then(function (r) { return r.ok ? r.text() : null; })
        .then(function (html) {
          if (!html) return;
          var doc = new DOMParser().parseFromString(html, "text/html");
          var fresh = doc.querySelector("[data-live]");
          var cur = document.querySelector("[data-live]");
          if (fresh && cur) { cur.innerHTML = fresh.innerHTML; if (window.dorangI18n) window.dorangI18n.refresh(); }
        })
        .catch(function () { /* a dropped poll is not an error worth showing */ })
        .then(function () {
          var cur = document.querySelector("[data-live]");
          if (cur) cur.classList.remove("stale");
          busy = false;
        });
    }, secs * 1000);
  }

})();
