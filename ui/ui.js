// agentd's UI (#41): every 30 seconds, or every 5 while a [data-live]
// section waits on a session's answer (#56), fetch the page again and
// swap in each [data-refresh] section, unless the owner is using it.
// A console (#59) polls its box instead: new entries come in at the
// bottom, a changed one is swapped in, a response is sent without
// leaving the page, and an entry opens its response form when clicked.
// The page carries the latest entries; older ones are fetched as the
// owner scrolls up, and the poll touches only the latest.
// Without this script the page is complete; a reload shows what is new.
(function () {
  "use strict";
  var every = 30000;
  var live = 5000;
  var poll = 4000;
  document.documentElement.classList.add("js");

  // busy says whether the owner is in section: focus inside it, a field
  // there they typed in or changed, or a response form open.
  function busy(section) {
    var a = document.activeElement;
    if (a && a !== document.body && section.contains(a)) {
      return true;
    }
    if (section.querySelector("details.respond[open]")) {
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

  // reopen opens in next the details open in old.
  function reopen(old, next) {
    var open = opened(old);
    var ds = next.querySelectorAll("details");
    for (var j = 0; j < ds.length; j++) {
      if (open[key(ds[j])]) {
        ds[j].setAttribute("open", "");
      }
    }
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
          reopen(old, next);
          old.replaceWith(document.importNode(next, true));
        }
      })
      .catch(function () {});
  }

  var box = document.getElementById("box");
  if (!box) {
    var tick = function () {
      refresh();
      setTimeout(tick, document.querySelector("[data-live]") ? live : every);
    };
    setTimeout(tick, document.querySelector("[data-live]") ? live : every);
    return;
  }

  var list = box.querySelector(".entries");
  var composer = document.getElementById("composer");
  var newer = document.querySelector(".box-new");
  var unseen = 0;
  var filter = "";
  var hashes = {};
  var replySrc = composer ? composer.querySelector('input[name="src"]').value : "";

  // pick sets select sel to value when it offers it, else to its first
  // option.
  function pick(sel, value) {
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value === value) {
        sel.selectedIndex = i;
        return true;
      }
    }
    sel.selectedIndex = 0;
    return false;
  }

  function atBottom() {
    return box.scrollHeight - box.scrollTop - box.clientHeight < 24;
  }
  function toBottom() {
    box.scrollTop = box.scrollHeight;
    unseen = 0;
    showNewer();
  }
  function showNewer() {
    if (!newer) {
      return;
    }
    newer.hidden = unseen === 0;
    newer.querySelector("button").textContent = unseen + " new ↓";
  }

  // element parses one entry's HTML.
  function element(html) {
    var t = document.createElement("template");
    t.innerHTML = html.trim();
    return t.content.firstElementChild;
  }

  // applyFilter shows only the entries of the session the chips filter
  // by, when one is chosen.
  function applyFilter() {
    box.classList.toggle("filtered", filter !== "");
    var es = list.querySelectorAll(".entry");
    for (var i = 0; i < es.length; i++) {
      es[i].classList.toggle("match", filter !== "" && es[i].getAttribute("data-session") === filter);
    }
    var cs = document.querySelectorAll(".session-chip");
    for (var j = 0; j < cs.length; j++) {
      cs[j].classList.toggle("active", cs[j].getAttribute("data-session") === filter);
    }
  }

  // merge brings the box up to the console's entries: a new one is put
  // in its place, a changed one swapped in unless the owner is in it.
  function merge(doc) {
    var bottom = atBottom();
    var prev = null;
    var empty = list.querySelector(".empty");
    var keep = {};
    for (var w = 0; w < doc.entries.length; w++) {
      keep[doc.entries[w].id] = true;
    }
    // The first of the window shown: what is before it, fetched on
    // scrolling up, the poll leaves alone.
    var shown0 = list.querySelectorAll(".entry");
    var first = null;
    for (var f = 0; f < shown0.length && !first; f++) {
      if (keep[shown0[f].id]) {
        first = shown0[f];
      }
    }
    for (var i = 0; i < doc.entries.length; i++) {
      var d = doc.entries[i];
      var el = document.getElementById(d.id);
      if (!d.html) {
        if (el) {
          prev = el; // as the page has it
        }
        continue;
      }
      if (el) {
        // Unknown until now, an entry is compared as the page parsed it.
        var changed = hashes[d.id] === undefined ? !element(d.html).isEqualNode(el) : hashes[d.id] !== d.hash;
        if (changed && !busy(el)) {
          var next = element(d.html);
          reopen(el, next);
          el.replaceWith(next);
          el = next;
          hashes[d.id] = d.hash;
        } else if (!changed) {
          hashes[d.id] = d.hash;
        }
      } else {
        el = element(d.html);
        hashes[d.id] = d.hash;
        if (empty) {
          empty.remove();
          empty = null;
        }
        if (prev && prev.nextSibling) {
          list.insertBefore(el, prev.nextSibling);
        } else if (prev) {
          list.appendChild(el);
        } else if (first) {
          list.insertBefore(el, first);
        } else {
          list.appendChild(el);
        }
        if (!bottom) {
          unseen++;
        }
      }
      prev = el;
    }
    // An entry of the window the console no longer holds goes, unless
    // the owner is in it.
    var shown = list.querySelectorAll(".entry");
    var inWindow = false;
    for (var k = 0; k < shown.length; k++) {
      inWindow = inWindow || keep[shown[k].id] === true;
      if (inWindow && !keep[shown[k].id] && !busy(shown[k])) {
        shown[k].remove();
        delete hashes[shown[k].id];
      }
    }
    openIDs = doc.open;
    var q = document.querySelector(".open-questions");
    if (q) {
      q.hidden = doc.open.length === 0;
      q.textContent = doc.open.length + " open question" + (doc.open.length === 1 ? "" : "s");
    }
    var chips = document.getElementById("chips");
    if (chips && doc.chips) {
      chips.replaceWith(element(doc.chips));
    }
    applyFilter();
    if (bottom) {
      toBottom();
    } else {
      showNewer();
    }
  }

  // have lists the entries the page holds as the server last sent them,
  // which it sends back without their HTML.
  function have() {
    var out = [];
    for (var id in hashes) {
      if (document.getElementById(id)) {
        out.push(id + "." + hashes[id]);
      }
    }
    return out.slice(-200).join(",");
  }

  function pull() {
    if (document.hidden) {
      return Promise.resolve();
    }
    return fetch(box.getAttribute("data-entries") + "?have=" + encodeURIComponent(have()), { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(merge)
      .catch(function () {});
  }

  // older fetches the entries before the first shown and puts them above
  // it, the box kept where the owner is; it says whether it fetched any.
  var loading = false;
  function older() {
    var link = box.querySelector(".older a");
    var top = list.querySelector(".entry");
    if (loading || !link || !top) {
      return Promise.resolve(false);
    }
    loading = true;
    var url = box.getAttribute("data-entries") + "?before=" + encodeURIComponent(top.id);
    return fetch(url, { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (doc) {
        var height = box.scrollHeight;
        for (var i = 0; i < doc.entries.length; i++) {
          var d = doc.entries[i];
          if (!document.getElementById(d.id)) {
            list.insertBefore(element(d.html), top);
            hashes[d.id] = d.hash;
          }
        }
        var head = list.querySelector(".entry");
        if (doc.more && head) {
          link.setAttribute("data-before", head.id);
          link.href = location.pathname + "?before=" + encodeURIComponent(head.id);
        } else {
          link.parentNode.remove();
        }
        applyFilter();
        box.scrollTop += box.scrollHeight - height;
        loading = false;
        return doc.entries.length > 0;
      })
      .catch(function () {
        loading = false;
        return false;
      });
  }

  // reveal shows entry id, fetching older entries until it is there.
  function reveal(id, tries) {
    var el = document.getElementById(id);
    if (el || tries <= 0) {
      return Promise.resolve(el);
    }
    return older().then(function (got) { return got ? reveal(id, tries - 1) : null; });
  }

  function loop() {
    pull().then(function () { setTimeout(loop, poll); });
  }

  // send posts a response form without leaving the page, then pulls the
  // box at once; a refusal is shown under the form.
  function send(form, submitter) {
    var data;
    try {
      data = new FormData(form, submitter);
    } catch (e) {
      data = new FormData(form);
      if (submitter && submitter.name) {
        data.append(submitter.name, submitter.value);
      }
    }
    var shown = form.querySelector(".composer-error");
    if (shown) {
      shown.remove();
    }
    fetch(form.action, { method: "POST", body: new URLSearchParams(data), credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) {
          return r.text().then(function (t) { return Promise.reject(t); });
        }
        var d = form.closest("details.respond");
        if (d) {
          d.removeAttribute("open");
        }
        var area = form.querySelector("textarea");
        if (area) {
          area.value = "";
        }
        if (form === composer) {
          clearRe();
        }
        return pull().then(toBottom);
      })
      .catch(function (t) {
        var p = document.createElement("p");
        p.className = "composer-error";
        p.textContent = typeof t === "string" ? t : "Not sent; try again.";
        form.appendChild(p);
      });
  }

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (form.getAttribute("action") !== "/respond") {
      return;
    }
    e.preventDefault();
    send(form, e.submitter);
  });

  // clearRe makes the composer respond to no entry.
  function clearRe() {
    if (!composer) {
      return;
    }
    composer.querySelector('input[name="entry"]').value = "";
    composer.querySelector('input[name="src"]').value = replySrc;
    composer.querySelector(".re-chip").hidden = true;
  }

  // reTo makes the composer respond to entry el and address its session.
  function reTo(el) {
    if (!composer) {
      return;
    }
    composer.querySelector('input[name="entry"]').value = el.getAttribute("data-entry");
    // The entry is found in the run that shows it.
    composer.querySelector('input[name="src"]').value = el.getAttribute("data-src") || replySrc;
    var head = el.querySelector(".msg-head");
    var body = el.querySelector(".md, .prompt-line, .question, p");
    var label = (head ? head.querySelector(".from").textContent + " " + (head.querySelector(".badge") || {}).textContent : "") +
      ": " + (body ? body.textContent.trim().slice(0, 80) : "");
    composer.querySelector(".re-label").textContent = label;
    composer.querySelector(".re-chip").hidden = false;
    var sel = composer.querySelector('select[name="session"]');
    if (sel) {
      pick(sel, el.getAttribute("data-to"));
    }
    composer.querySelector("textarea").focus();
  }

  var openIDs = [];
  var nextOpen = 0;

  document.addEventListener("click", function (e) {
    var t = e.target;
    if (t.closest(".reply-to")) {
      reTo(t.closest(".entry"));
      return;
    }
    if (t.closest(".re-clear")) {
      clearRe();
      return;
    }
    if (t.closest(".box-new button")) {
      toBottom();
      return;
    }
    if (t.closest(".older a")) {
      e.preventDefault();
      older();
      return;
    }
    if (t.closest(".open-questions")) {
      if (openIDs.length === 0) {
        return;
      }
      reveal(openIDs[nextOpen % openIDs.length], 20).then(function (el) {
        if (!el) {
          return;
        }
        el.scrollIntoView({ block: "center" });
        el.classList.add("flash");
        setTimeout(function () { el.classList.remove("flash"); }, 1200);
        var d = el.querySelector("details.respond");
        if (d) {
          d.setAttribute("open", "");
        }
      });
      nextOpen++;
      return;
    }
    var chip = t.closest(".chip-filter");
    if (chip) {
      var s = chip.getAttribute("data-session");
      filter = filter === s ? "" : s;
      applyFilter();
      var sel = composer && composer.querySelector('select[name="session"]');
      var to = chip.getAttribute("data-to");
      if (sel && to && filter !== "") {
        pick(sel, to);
      }
      return;
    }
    // A click on an entry, not on what it holds to use, opens or closes
    // its response form.
    var entry = t.closest(".entry[data-entry]");
    if (!entry || t.closest("a, button, input, textarea, select, label, summary, details.more, details.respond > form, details.respond > .default")) {
      return;
    }
    if (window.getSelection && String(window.getSelection()).length > 0) {
      return;
    }
    var r = entry.querySelector(":scope > details.respond");
    if (r) {
      if (r.hasAttribute("open")) {
        r.removeAttribute("open");
      } else {
        r.setAttribute("open", "");
        var area = r.querySelector("textarea");
        if (area) {
          area.focus({ preventScroll: true });
        }
      }
    }
  });

  if (composer) {
    composer.querySelector("textarea").addEventListener("keydown", function (e) {
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        send(composer, null);
      }
    });
  }
  box.addEventListener("scroll", function () {
    if (atBottom()) {
      unseen = 0;
      showNewer();
    }
    if (box.scrollTop < 200) {
      older();
    }
  });
  var q0 = document.querySelector(".open-questions");
  if (q0) {
    var ids = [];
    var os = list.querySelectorAll(".entry-open");
    for (var k = 0; k < os.length; k++) {
      ids.push(os[k].id);
    }
    openIDs = ids;
  }
  toBottom();
  // A window too short to scroll fetches older entries at once.
  if (box.scrollHeight <= box.clientHeight) {
    older();
  }
  setTimeout(loop, poll);
})();
