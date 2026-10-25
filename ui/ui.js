// agentd's UI (#41): every 30 seconds, fetch the page again and swap in
// each [data-refresh] section, unless the owner is using it. Without this
// script the page is complete; a reload shows what is new.
(function () {
  "use strict";
  var every = 30000;

  // busy says whether the owner is in section: focus inside it, or a
  // field there they typed in or changed.
  function busy(section) {
    var a = document.activeElement;
    if (a && a !== document.body && section.contains(a)) {
      return true;
    }
    var fields = section.querySelectorAll("textarea, input");
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i];
      if (f.type === "checkbox" || f.type === "radio") {
        if (f.checked !== f.defaultChecked) {
          return true;
        }
      } else if (f.type !== "hidden" && f.value !== f.defaultValue) {
        return true;
      }
    }
    return false;
  }

  // opened lists the summaries of the open details in section, so the
  // new section opens the same ones.
  function opened(section) {
    var out = {};
    var ds = section.querySelectorAll("details[open] > summary");
    for (var i = 0; i < ds.length; i++) {
      out[ds[i].textContent] = true;
    }
    return out;
  }

  function refresh() {
    if (document.hidden) {
      return;
    }
    fetch(location.href, { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        var sections = document.querySelectorAll("[data-refresh][id]");
        for (var i = 0; i < sections.length; i++) {
          var old = sections[i];
          var next = doc.getElementById(old.id);
          if (!next || busy(old)) {
            continue;
          }
          var open = opened(old);
          var ds = next.querySelectorAll("details > summary");
          for (var j = 0; j < ds.length; j++) {
            if (open[ds[j].textContent]) {
              ds[j].parentNode.setAttribute("open", "");
            }
          }
          old.replaceWith(document.importNode(next, true));
        }
      })
      .catch(function () {});
  }

  setInterval(refresh, every);
})();
