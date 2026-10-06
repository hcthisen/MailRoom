// Mailroom admin UI: the few behaviours htmx doesn't cover. No framework,
// no inline handlers (the CSP forbids them). Everything works without this
// file; it only adds conveniences.
(function () {
  "use strict";

  function flash(kind, text) {
    var box = document.getElementById("flashes");
    if (!box) return;
    var div = document.createElement("div");
    div.className = "flash flash-" + kind;
    div.setAttribute("role", kind === "error" ? "alert" : "status");
    var p = document.createElement("p");
    p.textContent = text;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "dismiss";
    btn.setAttribute("aria-label", "Dismiss");
    btn.textContent = "×";
    div.appendChild(p);
    div.appendChild(btn);
    box.appendChild(div);
  }

  // Dismiss buttons on flash messages.
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (t && t.classList && t.classList.contains("dismiss")) {
      var f = t.closest(".flash");
      if (f) f.remove();
    }
  });

  // Copy buttons: <button data-copy="#id"> (or data-copy="id") copies the
  // value (or text) of that element. Buttons, or their [data-copy-wrap]
  // wrapper, start hidden and are shown here, so without JavaScript there
  // is no dead button and the text can still be selected by hand.
  function copyTarget(btn) {
    var sel = btn.getAttribute("data-copy") || "";
    if (!sel) return null;
    return sel.charAt(0) === "#" ? document.querySelector(sel) : document.getElementById(sel);
  }
  function copyText(el) { return el.value !== undefined ? el.value : el.textContent; }
  function copied(btn, label) {
    var old = btn.getAttribute("data-label") || btn.textContent;
    btn.setAttribute("data-label", old);
    btn.textContent = label;
    setTimeout(function () { btn.textContent = old; }, 2000);
  }
  function copyFallback(el, btn) {
    if (el.select) { el.focus(); el.select(); }
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    copied(btn, ok ? "Copied" : "Press Ctrl+C");
  }
  function showCopyButtons(root) {
    (root || document).querySelectorAll("button[data-copy]").forEach(function (btn) {
      var wrap = btn.closest("[data-copy-wrap]");
      if (wrap) wrap.hidden = false;
      btn.hidden = false;
    });
  }
  document.addEventListener("click", function (e) {
    var btn = e.target && e.target.closest ? e.target.closest("button[data-copy]") : null;
    if (!btn) return;
    var el = copyTarget(btn);
    if (!el) return;
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(copyText(el)).then(function () { copied(btn, "Copied"); }, function () { copyFallback(el, btn); });
    } else {
      copyFallback(el, btn);
    }
  });
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { showCopyButtons(); });
  } else {
    showCopyButtons();
  }
  document.addEventListener("htmx:afterSwap", function (e) { showCopyButtons(e.target); });

  // htmx doesn't swap error responses; say something instead of nothing.
  document.addEventListener("htmx:responseError", function (e) {
    var status = e.detail && e.detail.xhr ? e.detail.xhr.status : 0;
    if (status === 401) {
      window.location.href = "/admin/login";
      return;
    }
    flash("error", "The request failed (" + status + "). Reload the page and try again.");
  });
  document.addEventListener("htmx:sendError", function () {
    flash("error", "The server could not be reached. Check your connection and try again.");
  });
})();
