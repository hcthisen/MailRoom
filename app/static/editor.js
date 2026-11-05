// Mailroom email editor (template "email-editor"). Progressive enhancement:
// without this file the editor is a format choice and a textarea, and the
// form still works. With it:
//   - three modes: Visual (a rich-text surface that writes HTML into the
//     textarea, marked <!-- mailroom:visual --> so the server styles it),
//     HTML code and Markdown, each with a formatting toolbar;
//   - a "Personalise" menu that inserts merge fields, with a fallback;
//   - an image picker (uploaded images, upload, or an address);
//   - "Preview as" stepping and a desktop/mobile preview;
//   - a warning before leaving with unsaved changes.
// No framework and no inline handlers (the CSP forbids them).
(function () {
  "use strict";

  var MARKER = "<!-- mailroom:visual -->";

  function csrfToken() {
    try {
      var h = JSON.parse(document.body.getAttribute("hx-headers") || "{}");
      return h["X-CSRF-Token"] || "";
    } catch (e) {
      return "";
    }
  }

  function fire(el, type) {
    el.dispatchEvent(new Event(type, { bubbles: true }));
  }

  function isVisual(html) {
    return html.replace(/^\s+/, "").indexOf(MARKER) === 0;
  }

  function stripMarker(html) {
    var s = html.replace(/^\s+/, "");
    return s.indexOf(MARKER) === 0 ? s.slice(MARKER.length).replace(/^\s+/, "") : html;
  }

  function escapeHTML(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  // ------------------------------------------------------------ paste cleaning

  var ALLOWED = {
    P: [], BR: [], H1: [], H2: [], H3: [], H4: [], STRONG: [], B: [], EM: [], I: [], U: [], S: [], STRIKE: [],
    A: ["href", "class"], UL: [], OL: [], LI: [], BLOCKQUOTE: [], HR: [], IMG: ["src", "alt", "width"],
    TABLE: [], THEAD: [], TBODY: [], TR: [], TD: [], TH: [], SPAN: [], DIV: [], CODE: [], PRE: []
  };
  var KEEP_STYLE = /^(color|text-align|font-weight|font-style|text-decoration)$/;

  function cleanNode(node, doc) {
    if (node.nodeType === 3) return doc.createTextNode(node.nodeValue);
    if (node.nodeType !== 1) return null;
    var tag = node.tagName;
    var frag = doc.createDocumentFragment();
    var kids = function (into) {
      for (var c = node.firstChild; c; c = c.nextSibling) {
        var k = cleanNode(c, doc);
        if (k) into.appendChild(k);
      }
      return into;
    };
    if (tag === "SCRIPT" || tag === "STYLE" || tag === "META" || tag === "LINK" || tag === "TITLE" || tag === "O:P") return null;
    if (!ALLOWED.hasOwnProperty(tag)) return kids(frag);
    var out = doc.createElement(tag === "DIV" ? "p" : tag.toLowerCase());
    ALLOWED[tag].forEach(function (a) {
      var v = node.getAttribute(a);
      if (v === null) return;
      if ((a === "href" || a === "src") && /^\s*(javascript|data|vbscript):/i.test(v)) return;
      out.setAttribute(a, v);
    });
    var st = node.getAttribute("style");
    if (st) {
      var keep = st.split(";").filter(function (d) {
        var p = d.split(":")[0];
        return p && KEEP_STYLE.test(p.trim().toLowerCase());
      }).join(";");
      if (keep) out.setAttribute("style", keep);
    }
    return kids(out);
  }

  function cleanHTML(html) {
    var doc = new DOMParser().parseFromString(html, "text/html");
    var box = document.createElement("div");
    for (var c = doc.body.firstChild; c; c = c.nextSibling) {
      var k = cleanNode(c, document);
      if (k) box.appendChild(k);
    }
    return box.innerHTML;
  }

  // ------------------------------------------------------------ merge-field chips
  // In the visual editor a merge field shows as a chip that can only be
  // deleted whole; the stored HTML keeps the plain {{field}} text.

  var MERGE_RX = /\{\{\s*([a-zA-Z_][\w.]*)\s*(?:\|\s*"([^"]*)"\s*)?\}\}/g;

  function chipLabel(name, fallback) {
    var labels = { first_name: "First name", last_name: "Last name", email: "Email", unsubscribe_url: "Unsubscribe link" };
    var l = labels[name] || (name.indexOf("attr.") === 0 ? name.slice(5).replace(/_/g, " ") : name);
    return fallback ? l + " · “" + fallback + "”" : l;
  }

  function chipHTML(tag) {
    MERGE_RX.lastIndex = 0;
    var m = MERGE_RX.exec(tag);
    if (!m) return escapeHTML(tag);
    return '<span class="mr-tag" contenteditable="false" data-tag="' + escapeHTML(tag) + '" title="' + escapeHTML(tag) + '">' + escapeHTML(chipLabel(m[1], m[2])) + "</span>";
  }

  // chipify turns merge fields in el's text into chips.
  function chipify(el) {
    var walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    var nodes = [];
    for (var n = walker.nextNode(); n; n = walker.nextNode()) {
      if (n.nodeValue.indexOf("{{") >= 0 && !n.parentNode.closest(".mr-tag")) nodes.push(n);
    }
    nodes.forEach(function (t) {
      var html = escapeHTML(t.nodeValue).replace(/\{\{[^}]*\}\}/g, function (m) {
        var raw = m.replace(/&quot;/g, '"').replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">");
        return chipHTML(raw);
      });
      var box = document.createElement("span");
      box.innerHTML = html;
      var frag = document.createDocumentFragment();
      while (box.firstChild) frag.appendChild(box.firstChild);
      t.parentNode.replaceChild(frag, t);
    });
  }

  // serialize returns el's HTML with chips back as {{field}} text.
  function serialize(el) {
    var clone = el.cloneNode(true);
    clone.querySelectorAll(".mr-tag").forEach(function (c) {
      c.replaceWith(document.createTextNode(c.getAttribute("data-tag") || ""));
    });
    return clone.innerHTML;
  }

  // ------------------------------------------------------------ editor

  function Editor(root) {
    this.root = root;
    this.form = root.closest("form");
    this.textarea = root.querySelector("#f-body");
    this.surface = root.querySelector("[data-surface]");
    this.toolbar = root.querySelector("[data-toolbar]");
    this.modes = root.querySelector("[data-modes]");
    this.radios = root.querySelector("[data-format-radios]");
    this.hint = root.querySelector("[data-body-hint]");
    this.mergePop = root.querySelector("[data-merge-popover]");
    this.imagePop = root.querySelector("[data-image-popover]");
    this.linkPop = root.querySelector("[data-link-popover]");
    this.range = null;
    this.mergeTarget = null;
    this.dirty = false;
    this.mode = "";
    this.init();
  }

  Editor.prototype.format = function () {
    var r = this.form.querySelector('input[name="format"]:checked');
    return r ? r.value : "html";
  };

  Editor.prototype.setFormat = function (v) {
    var r = this.form.querySelector('input[name="format"][value="' + v + '"]');
    if (r) r.checked = true;
  };

  Editor.prototype.init = function () {
    var self = this;
    this.radios.hidden = true;
    this.modes.hidden = false;
    this.toolbar.hidden = false;
    root(this);
    try {
      document.execCommand("styleWithCSS", false, true);
      document.execCommand("defaultParagraphSeparator", false, "p");
    } catch (e) { /* old browsers */ }

    var body = this.textarea.value;
    var mode = this.format() === "markdown" ? "markdown" : (body.trim() === "" || isVisual(body) ? "visual" : "html");
    this.show(mode);

    this.modes.addEventListener("click", function (e) {
      var b = e.target.closest("[data-mode]");
      if (b) self.switchTo(b.getAttribute("data-mode"));
    });

    this.surface.addEventListener("input", function () { self.saveRange(); self.sync(); });
    ["keyup", "mouseup"].forEach(function (ev) { self.surface.addEventListener(ev, function () { self.saveRange(); }); });
    this.surface.addEventListener("paste", function (e) { self.paste(e); });
    this.surface.addEventListener("keydown", function (e) { self.keys(e); });
    this.textarea.addEventListener("keydown", function (e) { self.keys(e); });
    document.addEventListener("selectionchange", function () { self.saveRange(); });
    this.surface.addEventListener("click", function (e) {
      var a = e.target.closest("a");
      if (a && self.surface.contains(a)) { e.preventDefault(); self.openLink(a); }
    });

    this.toolbar.addEventListener("mousedown", function (e) {
      // Keep the selection in the surface when a toolbar button is pressed.
      if (e.target.closest("button")) e.preventDefault();
    });
    this.toolbar.addEventListener("click", function (e) {
      var b = e.target.closest("button[data-cmd]");
      if (b) self.command(b.getAttribute("data-cmd"));
    });
    this.toolbar.addEventListener("change", function (e) {
      var t = e.target;
      if (t.matches("[data-cmd=block]")) { self.block(t.value); t.value = "p"; }
      if (t.matches("[data-cmd=foreColor]")) self.color(t.value);
    });

    this.root.addEventListener("click", function (e) {
      var m = e.target.closest("[data-merge-menu]");
      if (m) { self.openMerge(m); return; }
      var tag = e.target.closest("[data-merge-tag]");
      if (tag) { self.insertMerge(tag.getAttribute("data-merge-tag")); return; }
      var img = e.target.closest("[data-insert-image]");
      if (img) { self.insertImage(img.getAttribute("data-insert-image")); return; }
      if (e.target.closest("[data-image-url-insert]")) {
        var u = self.imagePop.querySelector("[data-image-url]");
        if (/^https?:\/\//i.test(u.value.trim())) { self.insertImage(u.value.trim()); u.value = ""; }
        else u.focus();
        return;
      }
      if (e.target.closest("[data-link-apply]")) { self.applyLink(); return; }
      if (e.target.closest("[data-link-remove]")) { self.removeLink(); return; }
      if (e.target.closest("[data-close]")) { self.closePopovers(); return; }
    });
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") self.closePopovers(); });
    document.addEventListener("click", function (e) {
      if (!e.target.closest(".popover") && !e.target.closest("[data-merge-menu],[data-cmd=image],[data-cmd=link],[data-cmd=button]")) self.closePopovers();
    });

    // Unsaved changes.
    this.form.addEventListener("input", function (e) {
      if (!e.target.closest(".preview-panel")) self.dirty = true;
    });
    this.form.addEventListener("submit", function () { self.sync(true); self.textarea.value = self.withMarker(); self.dirty = false; });
    // htmx requests (preview, test send) also get the marker back.
    this.form.addEventListener("htmx:configRequest", function (e) {
      if (!self.markerHidden) return;
      var p = e.detail.parameters, v = self.withMarker();
      if (p && typeof p.set === "function") p.set("body", v); else if (p) p.body = v;
    });
    window.addEventListener("beforeunload", function (e) {
      if (self.dirty) { e.preventDefault(); e.returnValue = ""; }
    });
  };

  function root(ed) { ed.root.classList.add("is-enhanced"); }

  // show displays mode without converting anything.
  Editor.prototype.show = function (mode) {
    this.mode = mode;
    // HTML code view of visual content: hide the marker comment; withMarker()
    // puts it back on every save and preview.
    this.markerHidden = false;
    if (mode === "html" && isVisual(this.textarea.value)) {
      this.textarea.value = stripMarker(this.textarea.value);
      this.markerHidden = true;
    }
    var visual = mode === "visual";
    if (visual) { this.surface.innerHTML = stripMarker(this.textarea.value); chipify(this.surface); }
    this.surface.hidden = !visual;
    this.textarea.hidden = visual;
    this.setFormat(mode === "markdown" ? "markdown" : "html");
    this.textarea.classList.toggle("code", mode !== "markdown");
    this.toolbar.setAttribute("data-mode", mode);
    this.modes.querySelectorAll("[data-mode]").forEach(function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-mode") === mode ? "true" : "false");
    });
    this.toolbar.querySelectorAll("[data-visual-only]").forEach(function (el) { el.hidden = !visual; });
    if (this.hint) {
      this.hint.textContent = mode === "markdown"
        ? "Markdown: **bold**, _italic_, # Heading, [link](https://…). Your layout (logo, footer, unsubscribe link) is added when it is sent."
        : mode === "html"
          ? "HTML as it is sent, inside your layout. Paste a design from another tool here."
          : "Your layout (logo, footer, unsubscribe link) is added when it is sent. Use Personalise to add the contact's name.";
    }
  };

  Editor.prototype.switchTo = function (mode) {
    var self = this;
    if (mode === this.mode) return;
    if (this.mode === "visual") this.sync(true);
    var body = this.textarea.value.trim();
    var from = this.mode;
    if (from === "markdown" && body !== "") {
      // Markdown → HTML on the server (the same converter the sender uses).
      var fd = new FormData();
      fd.append("body", this.textarea.value);
      fetch("/admin/editor/convert", { method: "POST", body: fd, credentials: "same-origin", headers: { "X-CSRF-Token": csrfToken() } })
        .then(function (r) { return r.text().then(function (t) { return { ok: r.ok, text: t }; }); })
        .then(function (res) {
          if (!res.ok) { window.alert("The Markdown could not be converted: " + res.text); return; }
          self.textarea.value = res.text;
          self.show(mode);
          self.changed();
        })
        .catch(function () { window.alert("The server could not be reached. Try again."); });
      return;
    }
    if (mode === "markdown" && body !== "") {
      if (!window.confirm("Markdown has no colours, alignment or buttons. Switch and keep only the text?")) return;
      var tmp = document.createElement("div");
      tmp.innerHTML = stripMarker(this.textarea.value);
      this.textarea.value = (tmp.innerText || tmp.textContent || "").replace(/\n{3,}/g, "\n\n").trim();
    }
    if (mode === "visual" && from === "html" && body !== "" && !isVisual(body) && !this.markerHidden) {
      if (/<table|<style|<html/i.test(body) && !window.confirm("This HTML looks like a finished design. The visual editor simplifies it and may change how it looks. Continue?")) return;
      this.textarea.value = MARKER + "\n" + cleanHTML(body);
    }
    this.show(mode);
    this.changed();
  };

  // withMarker is the body to send: the textarea, with the visual marker
  // put back when the HTML code view hides it.
  Editor.prototype.withMarker = function () {
    var v = this.textarea.value;
    return this.markerHidden && v.trim() !== "" && !isVisual(v) ? MARKER + "\n" + v : v;
  };

  // sync writes the surface into the textarea (visual mode).
  Editor.prototype.sync = function (quiet) {
    if (this.mode !== "visual") return;
    var html = serialize(this.surface).trim();
    var empty = this.surface.textContent.trim() === "" && !this.surface.querySelector("img,hr,a");
    var v = empty ? "" : MARKER + "\n" + html;
    if (v === this.textarea.value) return;
    this.textarea.value = v;
    if (!quiet) fire(this.textarea, "input");
  };

  Editor.prototype.changed = function () {
    this.dirty = true;
    fire(this.textarea, "input");
    fire(this.textarea, "change");
  };

  Editor.prototype.paste = function (e) {
    var cd = e.clipboardData;
    if (!cd) return;
    e.preventDefault();
    var html = cd.getData("text/html");
    if (html) {
      document.execCommand("insertHTML", false, cleanHTML(html));
    } else {
      var text = cd.getData("text/plain") || "";
      var parts = text.split(/\r?\n\r?\n/).map(function (p) { return "<p>" + escapeHTML(p).replace(/\r?\n/g, "<br>") + "</p>"; });
      document.execCommand("insertHTML", false, parts.length > 1 ? parts.join("") : escapeHTML(text).replace(/\r?\n/g, "<br>"));
    }
    this.sync();
  };

  Editor.prototype.keys = function (e) {
    if (!(e.ctrlKey || e.metaKey)) return;
    var k = e.key.toLowerCase();
    if (k === "k") { e.preventDefault(); this.command("link"); }
    if (this.mode !== "visual" && (k === "b" || k === "i")) { e.preventDefault(); this.command(k === "b" ? "bold" : "italic"); }
  };

  Editor.prototype.saveRange = function () {
    var sel = window.getSelection();
    if (sel && sel.rangeCount && this.surface.contains(sel.anchorNode)) this.range = sel.getRangeAt(0).cloneRange();
  };

  // insertInline puts html exactly at the saved caret, inside the text it
  // stands in (never after the block), and leaves the caret right after it,
  // so "Hi " + field + "," reads "Hi Sofie,".
  Editor.prototype.insertInline = function (html) {
    this.ensureParagraph();
    this.restoreRange();
    var sel = window.getSelection();
    var r = sel.rangeCount ? sel.getRangeAt(0) : null;
    if (!r || !this.surface.contains(r.startContainer)) {
      r = document.createRange();
      var last = this.surface.lastElementChild || this.surface;
      r.selectNodeContents(last);
      r.collapse(false);
    }
    r.deleteContents();
    var box = document.createElement("span");
    box.innerHTML = html;
    var node = box.firstChild;
    r.insertNode(node);
    var after = document.createRange();
    after.setStartAfter(node);
    after.collapse(true);
    sel.removeAllRanges();
    sel.addRange(after);
    this.range = after.cloneRange();
  };

  // ensureParagraph wraps loose text at the top level of the surface in
  // <p>, so every line is a paragraph like the rest.
  Editor.prototype.ensureParagraph = function () {
    var s = this.surface;
    if (!s.firstChild) {
      var p0 = document.createElement("p");
      p0.appendChild(document.createElement("br"));
      s.appendChild(p0);
      var r0 = document.createRange();
      r0.setStart(p0, 0);
      r0.collapse(true);
      this.range = r0;
      return;
    }
    var loose = [];
    for (var n = s.firstChild; n; n = n.nextSibling) {
      if (n.nodeType === 3 || (n.nodeType === 1 && /^(SPAN|B|STRONG|I|EM|U|S|A|BR)$/.test(n.tagName))) loose.push(n);
      else if (loose.length) break;
    }
    if (!loose.length || loose[0] !== s.firstChild) return;
    // Moving nodes resets live ranges to the parent: keep the caret where it was.
    var sc = this.range ? this.range.startContainer : null, so = this.range ? this.range.startOffset : 0;
    var p = document.createElement("p");
    s.insertBefore(p, loose[0]);
    loose.forEach(function (x) { p.appendChild(x); });
    if (sc && p.contains(sc)) {
      var r = document.createRange();
      r.setStart(sc, so);
      r.collapse(true);
      this.range = r;
    }
  };

  Editor.prototype.restoreRange = function () {
    this.surface.focus();
    if (!this.range) {
      var r = document.createRange();
      r.selectNodeContents(this.surface);
      r.collapse(false);
      this.range = r;
    }
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(this.range);
  };

  // ------------------------------------------------------------ commands

  Editor.prototype.command = function (cmd) {
    if (cmd === "link") { this.openLink(null); return; }
    if (cmd === "button") { this.openLink(null, true); return; }
    if (cmd === "image") { this.openImage(); return; }
    if (this.mode === "visual") {
      this.restoreRange();
      document.execCommand(cmd, false, null);
      this.sync();
      return;
    }
    var md = this.mode === "markdown";
    switch (cmd) {
      case "bold": this.wrapText(md ? "**" : "<strong>", md ? "**" : "</strong>", "bold text"); break;
      case "italic": this.wrapText(md ? "_" : "<em>", md ? "_" : "</em>", "italic text"); break;
      case "strikeThrough": this.wrapText(md ? "~~" : "<s>", md ? "~~" : "</s>", "text"); break;
      case "insertUnorderedList": this.prefixLines(md ? "- " : null, "ul"); break;
      case "insertOrderedList": this.prefixLines(md ? "1. " : null, "ol"); break;
      case "insertHorizontalRule": this.insertText(md ? "\n\n---\n\n" : "\n<hr>\n"); break;
    }
  };

  Editor.prototype.block = function (tag) {
    if (this.mode === "visual") {
      this.restoreRange();
      document.execCommand("formatBlock", false, "<" + tag + ">");
      this.sync();
      return;
    }
    if (this.mode === "markdown") {
      var p = { h1: "# ", h2: "## ", h3: "### ", blockquote: "> ", p: "" }[tag];
      this.prefixLines(p, null);
    } else {
      this.wrapText("<" + tag + ">", "</" + tag + ">", "text");
    }
  };

  Editor.prototype.color = function (c) {
    if (this.mode === "visual") {
      this.restoreRange();
      document.execCommand("foreColor", false, c);
      this.sync();
    } else if (this.mode === "html") {
      this.wrapText('<span style="color:' + c + '">', "</span>", "text");
    }
  };

  // Textarea helpers (HTML code and Markdown modes).
  Editor.prototype.wrapText = function (before, after, placeholder) {
    var t = this.textarea, s = t.selectionStart, e = t.selectionEnd;
    var sel = t.value.slice(s, e) || placeholder;
    t.setRangeText(before + sel + after, s, e, "end");
    t.selectionStart = s + before.length;
    t.selectionEnd = s + before.length + sel.length;
    t.focus();
    this.changed();
  };

  Editor.prototype.prefixLines = function (prefix, listTag) {
    var t = this.textarea, s = t.selectionStart, e = t.selectionEnd;
    var start = t.value.lastIndexOf("\n", s - 1) + 1;
    var text = t.value.slice(start, e) || "item";
    var out;
    if (prefix === null && listTag) {
      out = "<" + listTag + ">\n" + text.split("\n").map(function (l) { return "  <li>" + l + "</li>"; }).join("\n") + "\n</" + listTag + ">";
    } else {
      var n = 0;
      out = text.split("\n").map(function (l) {
        n++;
        var clean = l.replace(/^(#{1,6} |> |- |\d+\. )/, "");
        return (prefix === "1. " ? n + ". " : prefix) + clean;
      }).join("\n");
    }
    t.setRangeText(out, start, e, "end");
    t.focus();
    this.changed();
  };

  Editor.prototype.insertText = function (text) {
    var t = this.textarea;
    t.setRangeText(text, t.selectionStart, t.selectionEnd, "end");
    t.focus();
    this.changed();
  };

  // ------------------------------------------------------------ popovers

  Editor.prototype.closePopovers = function () {
    [this.mergePop, this.imagePop, this.linkPop].forEach(function (p) { p.hidden = true; });
  };

  Editor.prototype.place = function (pop, anchor) {
    this.closePopovers();
    pop.hidden = false;
    var r = anchor.getBoundingClientRect();
    var box = this.root.getBoundingClientRect();
    var left = Math.max(0, Math.min(r.left - box.left, box.width - pop.offsetWidth));
    pop.style.left = left + "px";
    pop.style.top = (r.bottom - box.top + 6) + "px";
  };

  Editor.prototype.openMerge = function (btn) {
    this.mergeTarget = btn.getAttribute("data-merge-menu");
    this.place(this.mergePop, btn);
    var list = this.mergePop.querySelector(".merge-list");
    if (window.htmx) window.htmx.trigger(list, "editor:load-fields");
    var first = this.mergePop.querySelector("[data-merge-fallback]");
    if (first) first.focus();
  };

  Editor.prototype.insertMerge = function (name) {
    var fb = this.mergePop.querySelector("[data-merge-fallback]");
    var fallback = fb ? fb.value.replace(/"/g, "").trim() : "";
    var tag = "{{" + name + (fallback && name !== "unsubscribe_url" ? ' | "' + fallback + '"' : "") + "}}";
    var target = this.mergeTarget;
    this.closePopovers();
    if (fb) fb.value = "";
    if (target === "f-body") {
      if (this.mode === "visual") {
        this.restoreRange();
        if (name === "unsubscribe_url") {
          this.restoreRange();
          document.execCommand("insertHTML", false, '<a href="{{unsubscribe_url}}">Unsubscribe</a>');
        } else {
          this.insertInline(chipHTML(tag));
        }
        this.sync();
      } else {
        this.insertText(tag);
      }
      return;
    }
    var input = document.getElementById(target);
    if (!input) return;
    input.setRangeText(tag, input.selectionStart, input.selectionEnd, "end");
    input.focus();
    fire(input, "input");
  };

  Editor.prototype.openImage = function () {
    this.place(this.imagePop, this.toolbar.querySelector("[data-cmd=image]"));
    var list = this.imagePop.querySelector("[data-media-list]");
    if (window.htmx) window.htmx.trigger(list, "editor:load-media");
  };

  Editor.prototype.insertImage = function (url) {
    this.closePopovers();
    var tag = '<img src="' + escapeHTML(url) + '" alt="" style="max-width:100%;height:auto">';
    if (this.mode === "visual") {
      this.restoreRange();
      document.execCommand("insertHTML", false, tag);
      this.sync();
    } else if (this.mode === "markdown") {
      this.insertText("![](" + url + ")");
    } else {
      this.insertText(tag);
    }
  };

  Editor.prototype.openLink = function (a, asButton) {
    this.editingLink = a;
    this.asButton = !!asButton || !!(a && a.classList.contains("mr-button"));
    var url = this.linkPop.querySelector("[data-link-url]");
    var text = this.linkPop.querySelector("[data-link-text]");
    var sel = "";
    if (this.mode === "visual") {
      if (!a && this.range) sel = this.range.toString();
    } else {
      sel = this.textarea.value.slice(this.textarea.selectionStart, this.textarea.selectionEnd);
    }
    url.value = a ? a.getAttribute("href") || "" : "";
    text.value = a ? a.textContent : sel;
    this.linkPop.querySelector("[data-link-title]").textContent = this.asButton ? (a ? "Edit button" : "Insert a button") : (a ? "Edit link" : "Insert a link");
    this.linkPop.querySelector("[data-link-remove]").hidden = !a;
    this.place(this.linkPop, a || this.toolbar.querySelector(asButton ? "[data-cmd=button]" : "[data-cmd=link]"));
    url.focus();
  };

  Editor.prototype.applyLink = function () {
    var url = this.linkPop.querySelector("[data-link-url]").value.trim();
    var text = this.linkPop.querySelector("[data-link-text]").value.trim();
    if (!url) { this.linkPop.querySelector("[data-link-url]").focus(); return; }
    if (!/^(https?:|mailto:|tel:|\{\{|#)/i.test(url)) url = "https://" + url;
    var label = text || (this.asButton ? "Read more" : url);
    var cls = this.asButton ? ' class="mr-button"' : "";
    var a = this.editingLink;
    this.closePopovers();
    if (a) {
      a.setAttribute("href", url);
      a.textContent = label;
      this.sync();
      return;
    }
    var html = '<a href="' + escapeHTML(url) + '"' + cls + ">" + escapeHTML(label) + "</a>";
    if (this.asButton) html = '<p style="text-align:center">' + html + "</p>";
    if (this.mode === "visual") {
      if (this.asButton) { this.insertBlock(html); return; }
      this.restoreRange();
      document.execCommand("insertHTML", false, html);
      this.sync();
    } else if (this.mode === "markdown" && !this.asButton) {
      this.insertText("[" + label + "](" + url + ")");
    } else {
      this.insertText(this.asButton ? "\n" + html + "\n" : html);
    }
  };

  // insertBlock puts html after the block that holds the caret (a button
  // is its own centred paragraph, never merged into the text).
  Editor.prototype.insertBlock = function (html) {
    this.restoreRange();
    var node = this.range ? this.range.startContainer : null;
    if (node === this.surface) node = this.surface.childNodes[this.range.startOffset - 1] || null;
    while (node && node.parentNode !== this.surface) node = node.parentNode;
    var box = document.createElement("div");
    box.innerHTML = html;
    var nodes = Array.prototype.slice.call(box.childNodes);
    var anchor = node;
    nodes.forEach(function (n) {
      if (anchor) { anchor.after(n); } else { this.surface.appendChild(n); }
      anchor = n;
    }, this);
    if (!anchor.nextSibling) {
      var p = document.createElement("p");
      p.appendChild(document.createElement("br"));
      anchor.after(p);
    }
    var r = document.createRange();
    r.setStartAfter(anchor);
    r.collapse(true);
    this.range = r;
    this.sync();
  };

  Editor.prototype.removeLink = function () {
    var a = this.editingLink;
    this.closePopovers();
    if (!a) return;
    a.replaceWith(document.createTextNode(a.textContent));
    this.sync();
  };

  // ------------------------------------------------------------ preview panel

  function initPreview(root) {
    var panel = root.querySelector(".preview-panel");
    if (!panel) return;
    var devices = panel.querySelector("[data-devices]");
    if (devices) {
      devices.hidden = false;
      devices.addEventListener("click", function (e) {
        var b = e.target.closest("[data-device]");
        if (!b) return;
        devices.querySelectorAll("[data-device]").forEach(function (x) { x.setAttribute("aria-pressed", x === b ? "true" : "false"); });
        panel.classList.toggle("is-mobile", b.getAttribute("data-device") === "mobile");
      });
    }
    var select = panel.querySelector("#preview-contact");
    panel.addEventListener("click", function (e) {
      var s = e.target.closest("[data-contact-step]");
      if (!s || !select) return;
      var n = select.options.length;
      if (n < 2) return;
      var i = select.selectedIndex + parseInt(s.getAttribute("data-contact-step"), 10);
      // Step through the real contacts (index 0 is "no contact").
      if (i < 1) i = n - 1;
      if (i >= n || select.options[i].disabled) i = 1;
      select.selectedIndex = i;
      fire(select, "change");
    });
    if (select) {
      select.addEventListener("htmx:afterSwap", function () {
        var form = root.closest("form");
        if (form) form.dispatchEvent(new Event("editor:refresh"));
      });
    }
  }

  function initAll(scope) {
    (scope || document).querySelectorAll("[data-editor]").forEach(function (el) {
      if (el._editor) return;
      el._editor = new Editor(el);
      initPreview(el);
    });
    // A preview of stored content (a broadcast's page) without an editor.
    (scope || document).querySelectorAll("[data-preview-root]").forEach(function (el) {
      if (el._preview) return;
      el._preview = true;
      el.addEventListener("submit", function (e) { e.preventDefault(); });
      initPreview(el);
    });
  }

  // ------------------------------------------------------------ pickers outside the editor
  // [data-image-target="input-id"]: "Choose from your images" fills that input.
  // input[type=color][data-color-for="input-id"]: a swatch synced with a hex field.
  function initPickers() {
    document.querySelectorAll("[data-image-target]").forEach(function (box) {
      var input = document.getElementById(box.getAttribute("data-image-target"));
      var pop = box.querySelector("[data-image-pop]");
      var thumb = box.querySelector("[data-image-thumb]");
      if (!input || !pop) return;
      var showThumb = function () {
        if (!thumb) return;
        var v = input.value.trim();
        thumb.hidden = !/^https?:\/\//i.test(v);
        if (!thumb.hidden) thumb.src = v;
      };
      showThumb();
      input.addEventListener("input", showThumb);
      box.addEventListener("click", function (e) {
        if (e.target.closest("[data-image-toggle]")) {
          pop.hidden = !pop.hidden;
          if (!pop.hidden && window.htmx) window.htmx.trigger(pop.querySelector("[data-media-list]"), "editor:load-media");
          return;
        }
        if (e.target.closest("[data-close]")) { pop.hidden = true; return; }
        var img = e.target.closest("[data-insert-image]");
        if (img) {
          input.value = img.getAttribute("data-insert-image");
          pop.hidden = true;
          fire(input, "input");
          fire(input, "change");
        }
      });
    });
    document.querySelectorAll("input[type=color][data-color-for]").forEach(function (sw) {
      var input = document.getElementById(sw.getAttribute("data-color-for"));
      if (!input) return;
      sw.hidden = false;
      var sync = function () { if (/^#[0-9a-f]{6}$/i.test(input.value.trim())) sw.value = input.value.trim(); };
      sync();
      input.addEventListener("input", sync);
      sw.addEventListener("input", function () { input.value = sw.value; fire(input, "input"); });
    });
  }
  // Email previews (iframe[data-autosize], sandboxed without scripts but
  // same-origin so it can be measured) grow to their content's height.
  function autosize(frame) {
    try {
      var doc = frame.contentDocument;
      if (!doc || !doc.documentElement) return;
      var h = Math.max(doc.documentElement.scrollHeight, doc.body ? doc.body.scrollHeight : 0);
      frame.style.height = Math.max(320, Math.min(h + 8, 4000)) + "px";
    } catch (e) { /* not measurable: keep the CSS height */ }
  }
  document.addEventListener("load", function (e) {
    if (e.target && e.target.matches && e.target.matches("iframe[data-autosize]")) autosize(e.target);
  }, true);
  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-device]")) {
      setTimeout(function () { document.querySelectorAll("iframe[data-autosize]").forEach(autosize); }, 250);
    }
    // "Send test" in the broadcast checklist opens the test box below.
    if (e.target.closest("[data-open-test]")) {
      var box = document.querySelector("[data-test-box]");
      if (box) {
        box.open = true;
        box.scrollIntoView({ behavior: "smooth", block: "center" });
        var to = box.querySelector("#test-to");
        if (to) setTimeout(function () { to.focus(); }, 300);
      }
    }
  });

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", initPickers);
  else initPickers();

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", function () { initAll(); });
  else initAll();
})();
