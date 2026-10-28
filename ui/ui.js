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

  // The conversation of a level's console box (#69) keeps its newest
  // entry last, so it is brought to its bottom: on load, and after a
  // refresh unless the owner had scrolled up in it.
  function atBottom(el) {
    return el.scrollHeight - el.scrollTop - el.clientHeight < 8;
  }
  function talkBottom(root) {
    var talks = root.querySelectorAll(".talk");
    for (var i = 0; i < talks.length; i++) {
      talks[i].scrollTop = talks[i].scrollHeight;
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
        // Each swapped section's scroll position, and that of the
        // scrolling list inside it, by the section's id.
        var scrolled = {};
        for (var i = 0; i < sections.length; i++) {
          var old = sections[i];
          var next = doc.getElementById(old.id);
          if (!next || busy(old)) {
            continue;
          }
          reopen(old, next);
          var inner = old.querySelector(".talk");
          scrolled[old.id] = { top: old.scrollTop, inner: inner ? inner.scrollTop : 0, atBottom: !inner || atBottom(inner) };
          old.replaceWith(document.importNode(next, true));
        }
        levelBoxes();
        // After the sort, so the box shows the rows the owner scrolled to.
        Object.keys(scrolled).forEach(function (id) {
          var el = document.getElementById(id);
          if (!el) { return; }
          if (scrolled[id].top > 0) { el.scrollTop = scrolled[id].top; }
          // The conversation stays at its newest entry, unless the owner
          // scrolled up in it to read.
          if (scrolled[id].atBottom) {
            talkBottom(el);
          } else {
            var inner = el.querySelector(".talk");
            if (inner) { inner.scrollTop = scrolled[id].inner; }
          }
        });
      })
      .catch(function () {});
  }

  // The level boxes (#69): the issue box and the MR box sort and filter
  // their rows here, over the rows' data, so sorting never fetches; the
  // choice is kept per repository and box, and applied again after a
  // refresh swaps a box in.
  function stored(key) {
    try { return JSON.parse(localStorage.getItem(key) || "{}"); } catch (e) { return {}; }
  }
  function store(key, v) {
    try { localStorage.setItem(key, JSON.stringify(v)); } catch (e) {}
  }
  // box sorts: begin. The sorts are levels.go's sortIssues and
  // sortRequests, and a test runs this part against them on the same
  // rows: text compares with A-Z as a-z, character by character by code
  // point, as Go compares UTF-8 bytes, the same in every locale; none is
  // last; ties go to the latest change, then the highest number.
  var wordRank = { "needs you": 0, "working": 1, "stopped": 2 };
  function num(row, k) { return Number(row.dataset[k] || 0); }
  function text(row, k) {
    return (row.dataset[k] || "\udbff\udfff").replace(/[A-Z]/g, function (c) { return c.toLowerCase(); });
  }
  function cmp(x, y) {
    var i = 0, j = 0;
    while (i < x.length && j < y.length) {
      var a = x.codePointAt(i), b = y.codePointAt(j);
      if (a !== b) { return a < b ? -1 : 1; }
      i += a > 0xffff ? 2 : 1;
      j += b > 0xffff ? 2 : 1;
    }
    return i < x.length ? 1 : j < y.length ? -1 : 0;
  }
  function rank(row) { return wordRank[row.dataset.word] !== undefined ? wordRank[row.dataset.word] : 3; }
  // run is how long ago a worked row's session started, the longest first.
  function run(row) { return num(row, "run") || Infinity; }
  var sorts = {
    "needs-me": function (a, b) { return rank(a) - rank(b); },
    "last-change": function () { return 0; },
    "working": function (a, b) {
      return (b.dataset.working ? 1 : 0) - (a.dataset.working ? 1 : 0) || (a.dataset.working ? cmp(run(a), run(b)) : 0);
    },
    "has-mr": function (a, b) {
      var x = num(a, "mr") > 0, y = num(b, "mr") > 0;
      return (y ? 1 : 0) - (x ? 1 : 0) || (x ? num(a, "mrrank") - num(b, "mrrank") : 0);
    },
    "newest": function (a, b) { return num(b, "number") - num(a, "number"); },
    "oldest": function (a, b) { return num(a, "number") - num(b, "number"); },
    "stale": function (a, b) { return num(a, "changed") - num(b, "changed"); },
    "label": function (a, b) { return cmp(text(a, "label"), text(b, "label")); },
    "milestone": function (a, b) { return cmp(text(a, "milestone"), text(b, "milestone")); },
    "author": function (a, b) { return cmp(text(a, "author"), text(b, "author")); },
    "loop": function (a, b) { return num(a, "rank") - num(b, "rank"); },
    "ci-failing": function (a, b) { return (b.dataset.failing ? 1 : 0) - (a.dataset.failing ? 1 : 0); }
  };
  function compareRows(by, a, b) {
    var c = sorts[by] ? sorts[by](a, b) : 0;
    if (c === 0) { c = num(b, "changed") - num(a, "changed"); }
    if (c === 0) { c = num(b, "number") - num(a, "number"); }
    return c;
  }
  // box sorts: end
  function matches(row, words) {
    var hay = row.dataset.text || row.textContent.toLowerCase();
    var refs = (row.dataset.refs || row.dataset.ref || "").split(" ");
    for (var i = 0; i < words.length; i++) {
      var w = words[i];
      if (w.charAt(0) === "#" || w.charAt(0) === "!") {
        if (refs.indexOf(w) < 0) { return false; }
      } else if (hay.indexOf(w) < 0) {
        return false;
      }
    }
    return true;
  }
  function applyBox(section) {
    var key = "agentd-box:" + section.dataset.key + ":" + section.dataset.box;
    var state = stored(key);
    var select = section.querySelector(".box-sort");
    var filter = section.querySelector(".box-filter");
    if (select && state.sort && sorts[state.sort]) { select.value = state.sort; }
    if (filter && typeof state.filter === "string" && document.activeElement !== filter) {
      // As the page's own value, so a refresh does not take the box for
      // one the owner is typing in.
      filter.value = state.filter;
      filter.defaultValue = state.filter;
    }
    var by = select ? select.value : "";
    var words = filter ? filter.value.toLowerCase().split(/\s+/).filter(Boolean) : [];
    var list = section.querySelector(".box-rows");
    var rows = Array.prototype.slice.call(list.querySelectorAll(".box-row"));
    rows.sort(function (a, b) { return compareRows(by, a, b); });
    var shown = 0;
    rows.forEach(function (r) {
      r.hidden = !matches(r, words);
      if (!r.hidden) { shown++; }
      list.appendChild(r);
    });
    var out = section.querySelector(".box-shown");
    if (out) { out.textContent = String(shown); }
  }
  function levelBoxes() {
    var sections = document.querySelectorAll("[data-box]");
    for (var i = 0; i < sections.length; i++) {
      applyBox(sections[i]);
    }
  }
  function boxInput(e) {
    var section = e.target.closest ? e.target.closest("[data-box]") : null;
    if (!section || !e.target.matches(".box-sort, .box-filter")) {
      return;
    }
    var key = "agentd-box:" + section.dataset.key + ":" + section.dataset.box;
    var select = section.querySelector(".box-sort");
    var filter = section.querySelector(".box-filter");
    store(key, { sort: select ? select.value : "", filter: filter ? filter.value : "" });
    if (filter) { filter.defaultValue = filter.value; }
    applyBox(section);
  }
  document.addEventListener("change", boxInput);
  document.addEventListener("input", boxInput);
  // A link may name the issue box's sort, as home's waits-on-you number
  // does (?sort=needs-me): it is taken as the owner's choice.
  (function () {
    var m = /[?&]sort=([a-z-]+)/.exec(location.search);
    var issues = document.querySelector('[data-box="issues"]');
    var select = issues ? issues.querySelector(".box-sort") : null;
    if (!m || !select || !select.querySelector('option[value="' + m[1] + '"]')) { return; }
    // Once: a reload keeps what the owner chooses after.
    try { history.replaceState(null, "", location.pathname + location.hash); } catch (e) {}
    var key = "agentd-box:" + issues.dataset.key + ":issues";
    var state = stored(key);
    state.sort = m[1];
    store(key, state);
    select.value = m[1];
  })();
  levelBoxes();
  talkBottom(document);

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
    // None of the window shown: more came than a window while the page
    // slept, and the box starts again from the latest.
    if (!first && shown0.length > 0 && doc.entries.length > 0) {
      for (var g = 0; g < shown0.length; g++) {
        shown0[g].remove();
      }
      hashes = {};
      olderLink(doc.more, null);
      bottom = true;
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

  // have lists the latest entries the page holds as the server last
  // sent them, which it sends back without their HTML.
  function have() {
    var out = [];
    var es = list.querySelectorAll(".entry");
    for (var i = Math.max(es.length - 60, 0); i < es.length; i++) {
      if (hashes[es[i].id]) {
        out.push(es[i].id + "." + hashes[es[i].id]);
      }
    }
    return out.join(",");
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

  // olderLink shows the link to older entries when there are some, to
  // those before entry head, or hides it.
  function olderLink(more, head) {
    var p = box.querySelector(".older");
    if (!more) {
      if (p) {
        p.remove();
      }
      return;
    }
    if (!p) {
      p = document.createElement("p");
      p.className = "older";
      p.appendChild(document.createElement("a")).textContent = "Older entries";
      box.insertBefore(p, list);
    }
    var a = p.querySelector("a");
    a.href = head ? location.pathname + "?before=" + encodeURIComponent(head.id) : location.pathname;
    a.setAttribute("data-before", head ? head.id : "");
  }

  // older fetches the entries before the first shown and puts them above
  // it, the box kept where the owner is; it says whether it fetched any.
  // A fetch already under way is the one waited for.
  var loading = null;
  function older() {
    var link = box.querySelector(".older a");
    var top = list.querySelector(".entry");
    if (loading) {
      return loading;
    }
    if (!link || !top) {
      return Promise.resolve(false);
    }
    var url = box.getAttribute("data-entries") + "?before=" + encodeURIComponent(top.id) + "&at=" + (top.getAttribute("data-at") || "");
    loading = fetch(url, { credentials: "same-origin", cache: "no-store" })
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
        olderLink(doc.more, list.querySelector(".entry"));
        applyFilter();
        box.scrollTop += box.scrollHeight - height;
        loading = null;
        return doc.entries.length > 0;
      })
      .catch(function () {
        loading = null;
        return false;
      });
    return loading;
  }

  // refresh brings an entry above the latest window up to date, as one
  // just answered: the window before the entry below it is fetched again.
  function refreshAt(el) {
    var below = el.nextElementSibling;
    if (!below) {
      return Promise.resolve();
    }
    var url = box.getAttribute("data-entries") + "?before=" + encodeURIComponent(below.id) + "&at=" + (below.getAttribute("data-at") || "");
    return fetch(url, { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (doc) {
        for (var i = 0; i < doc.entries.length; i++) {
          var d = doc.entries[i];
          var old = document.getElementById(d.id);
          if (old && d.html && hashes[d.id] !== d.hash && !busy(old)) {
            old.replaceWith(element(d.html));
            hashes[d.id] = d.hash;
          }
        }
      })
      .catch(function () {});
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
    // A form with files goes as multipart (#61), one without as before.
    var picked = form.querySelector('input[type="file"]');
    var body = picked && picked.files.length ? data : new URLSearchParams(data);
    fetch(form.action, { method: "POST", body: body, credentials: "same-origin" })
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
        if (window.agentdAttach) {
          window.agentdAttach.clear(form);
        }
        if (form === composer) {
          clearRe();
        }
        var entry = form.closest(".entry");
        return pull().then(function () { return entry ? refreshAt(entry) : null; }).then(toBottom);
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
  var held = list.querySelectorAll(".entry[data-hash]");
  for (var h = 0; h < held.length; h++) {
    hashes[held[h].id] = held[h].getAttribute("data-hash");
  }
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

// Attachments (#61): a form with a file input takes screenshots, PDFs and
// text files pasted into it (Ctrl+V, the main case), dropped on it, or
// picked with its button; a paste anywhere else on a console goes to its
// composer. The files are checked here as agentd checks them, so a wrong
// one is refused at once, and agentd checks them again. The input holds
// them, so the form sends them however it is sent.
(function () {
  "use strict";
  var types = ["image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf", "text/plain"];
  var maxBytes = 20 * 1024 * 1024;
  var maxFiles = 10;
  var held = new WeakMap();

  function inputOf(form) {
    return form ? form.querySelector('.attach input[type="file"]') : null;
  }

  function filesOf(form) {
    var fs = held.get(form);
    if (!fs) {
      fs = [];
      held.set(form, fs);
    }
    return fs;
  }

  // textFile: a text file of any name, a log say, which a browser may
  // call text/x-log, or nothing; agentd reads its bytes to be sure.
  function textFile(f) {
    return f.type.indexOf("text/") === 0 || f.type === "" && /\.(txt|log)$/i.test(f.name);
  }

  // same says whether two files are one picked again.
  function same(a, b) {
    return a.name === b.name && a.size === b.size && a.lastModified === b.lastModified;
  }

  // say shows a refusal under the form, or clears it.
  function say(form, text) {
    var p = form.querySelector(".attach-error");
    if (!text) {
      if (p) {
        p.remove();
      }
      return;
    }
    if (!p) {
      p = document.createElement("p");
      p.className = "composer-error attach-error";
      form.appendChild(p);
    }
    p.textContent = text;
  }

  // sync puts the held files into the input and lists them; the text is
  // not needed while a file is.
  function sync(form) {
    var input = inputOf(form);
    var fs = filesOf(form);
    var dt = new DataTransfer();
    fs.forEach(function (f) { dt.items.add(f); });
    input.files = dt.files;
    var area = form.querySelector("textarea");
    if (area) {
      if (!area.hasAttribute("data-required")) {
        area.setAttribute("data-required", area.required ? "1" : "");
      }
      area.required = area.getAttribute("data-required") === "1" && fs.length === 0;
    }
    var list = form.querySelector(".attach-list");
    Array.prototype.forEach.call(list.querySelectorAll("img"), function (img) { URL.revokeObjectURL(img.src); });
    list.textContent = "";
    fs.forEach(function (f, i) {
      var li = document.createElement("li");
      if (f.type.indexOf("image/") === 0) {
        var img = document.createElement("img");
        img.src = URL.createObjectURL(f);
        img.alt = "";
        li.appendChild(img);
      }
      var name = document.createElement("span");
      name.textContent = f.name + " (" + Math.max(1, Math.round(f.size / 1024)) + " KB)";
      li.appendChild(name);
      var x = document.createElement("button");
      x.type = "button";
      x.className = "link attach-remove";
      x.setAttribute("aria-label", "Remove " + f.name);
      x.textContent = "×";
      x.addEventListener("click", function () {
        fs.splice(i, 1);
        sync(form);
      });
      li.appendChild(x);
      list.appendChild(li);
    });
  }

  // add holds files for form, after checking each; one refused is named
  // and the rest are kept.
  function add(form, files) {
    var fs = filesOf(form);
    var refused = [];
    Array.prototype.forEach.call(files, function (f) {
      var name = f.name || "pasted";
      if (types.indexOf(f.type) < 0 && !textFile(f)) {
        refused.push(name + " is not a PNG, JPEG, GIF, WebP, PDF or plain text file");
      } else if (f.size > maxBytes) {
        refused.push(name + " is over 20 MB");
      } else if (f.size === 0) {
        refused.push(name + " is empty");
      } else if (fs.length >= maxFiles) {
        refused.push("a message carries at most " + maxFiles + " files");
      } else {
        // A pasted screenshot is named for when it was pasted.
        if (!f.name || f.name === "image.png") {
          var stamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-");
          f = new File([f], "screenshot-" + stamp + "." + (f.type.split("/")[1] || "png"), { type: f.type });
        }
        fs.push(f);
      }
    });
    say(form, refused.length ? "Not attached: " + refused.join("; ") + "." : "");
    sync(form);
  }

  window.agentdAttach = {
    clear: function (form) {
      if (inputOf(form)) {
        held.set(form, []);
        say(form, "");
        sync(form);
      }
    },
  };

  document.addEventListener("change", function (e) {
    var input = e.target;
    if (!input.matches || !input.matches('.attach input[type="file"]')) {
      return;
    }
    var picked = Array.prototype.slice.call(input.files);
    var form = input.form;
    // The input now holds only what was picked: the held files go back
    // in with it.
    add(form, picked.filter(function (f) {
      return !filesOf(form).some(function (g) { return same(f, g); });
    }));
  });

  document.addEventListener("paste", function (e) {
    var files = e.clipboardData && e.clipboardData.files;
    if (!files || !files.length) {
      return;
    }
    var form = e.target.closest ? e.target.closest("form") : null;
    if (!inputOf(form)) {
      form = document.getElementById("composer");
    }
    if (!inputOf(form)) {
      return;
    }
    // Text pasted with an image (from a document, say) still pastes.
    if (!e.clipboardData.getData("text/plain")) {
      e.preventDefault();
    }
    add(form, files);
    var area = form.querySelector("textarea");
    if (area && document.activeElement !== area) {
      area.focus();
    }
  });

  document.addEventListener("dragover", function (e) {
    var form = e.target.closest ? e.target.closest("form") : null;
    if (inputOf(form) && e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types, "Files") >= 0) {
      e.preventDefault();
      form.classList.add("attach-over");
    }
  });
  document.addEventListener("dragleave", function (e) {
    var form = e.target.closest ? e.target.closest("form") : null;
    if (form) {
      form.classList.remove("attach-over");
    }
  });
  document.addEventListener("drop", function (e) {
    var form = e.target.closest ? e.target.closest("form") : null;
    if (!inputOf(form) || !e.dataTransfer || !e.dataTransfer.files.length) {
      return;
    }
    e.preventDefault();
    form.classList.remove("attach-over");
    add(form, e.dataTransfer.files);
  });
})();
