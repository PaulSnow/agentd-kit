// agentd's UI (#41): every 30 seconds, or every 5 while a [data-live]
// section waits on a session's answer (#56), fetch the page again and
// swap in each [data-refresh] section, unless the owner is using it.
// Without this script the page is complete; a reload shows what is new.
(function () {
  "use strict";
  var every = 30000;
  var live = 5000;

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

  // key names a details element across refreshes: its data-key, which
  // stays when its count changes, else its summary's text.
  function key(d) {
    var s = d.querySelector("summary");
    return d.getAttribute("data-key") || (s ? s.textContent : "");
  }

  // opened lists the open details in section, so the new section opens
  // the same ones.
  function opened(section) {
    var out = {};
    var ds = section.querySelectorAll("details[open]");
    for (var i = 0; i < ds.length; i++) {
      out[key(ds[i])] = true;
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
          var ds = next.querySelectorAll("details");
          for (var j = 0; j < ds.length; j++) {
            if (open[key(ds[j])]) {
              ds[j].setAttribute("open", "");
            }
          }
          old.replaceWith(document.importNode(next, true));
        }
      })
      .catch(function () {});
  }

  function tick() {
    refresh();
    setTimeout(tick, document.querySelector("[data-live]") ? live : every);
  }
  setTimeout(tick, document.querySelector("[data-live]") ? live : every);
})();
