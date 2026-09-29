package main

// The page answers two questions and nothing else: is the plugin active, and
// which models can I use. Everything else it used to show was either an
// implementation detail (the request contract, the tool-name list, the user
// agent version) or a one-time setup artifact (the provider block), so the
// provider block is collapsed and the rest is gone.
const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>OpenCode Free</title>
<link rel="stylesheet" href="?asset=css">
</head>
<body>
<main>
  <header class="head">
    <div>
      <h1>OpenCode Free</h1>
      <p class="sub">Serves the keyless free tier through CPA. Use the model names below as
      plain model names; no prefix and no key needed.</p>
    </div>
    <span id="state" class="pill">loading</span>
  </header>

  <section class="card">
    <h2>Models</h2>
    <div id="models" class="models"></div>
    <p class="hint" id="models-hint"></p>
  </section>

  <section class="card">
    <details id="setup">
      <summary>Setting up a new host</summary>
      <p class="hint">Paste into that host's CPA <code>config.yaml</code>. The empty pool entries
      give rotation independent auth records; the plugin handles the request side.</p>
      <pre id="provider" class="code">loading…</pre>
      <div class="row">
        <button class="btn" id="copy-provider">Copy block</button>
        <a class="btn ghost" href="?asset=config" download="ocfree.yaml">Download config</a>
        <span id="msg" class="msg"></span>
      </div>
    </details>
  </section>
</main>
<script src="?asset=js"></script>
</body>
</html>`

const pageCSS = `
:root{
  --bg:var(--app-bg,#fff); --surface:var(--app-surface,#fff);
  --surface-2:var(--app-surface-muted,#f7f9fc); --border:var(--app-border,rgba(15,23,42,.09));
  --text:var(--app-text-primary,#2c3e50); --text-2:var(--app-text-regular,#5f6c7b);
  --muted:var(--app-text-muted,#8b95a6); --ok:var(--app-success,#16a34a);
  --accent:var(--app-accent,#2563eb);
}
@media (prefers-color-scheme:dark){
  :root{ --bg:var(--app-bg,#0d1117); --surface:var(--app-surface,#0d1117);
    --surface-2:var(--app-surface-muted,#161b22); --border:var(--app-border,rgba(255,255,255,.10));
    --text:var(--app-text-primary,#e6e9ef); --text-2:var(--app-text-regular,#b6bdc9);
    --muted:var(--app-text-muted,#8b95a6); --ok:var(--app-success,#4ade80);
    --accent:var(--app-accent,#60a5fa); }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
  font:14px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:900px;margin:0 auto;padding:24px 20px 60px}
.head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:18px}
h1{margin:0;font-size:20px;font-weight:650;letter-spacing:-.01em}
h2{margin:0 0 4px;font-size:13px;font-weight:600;color:var(--text-2);
  text-transform:uppercase;letter-spacing:.06em}
.sub{margin:4px 0 0;color:var(--muted);font-size:13px;max-width:62ch}
.pill{font-size:11px;padding:3px 9px;border-radius:999px;white-space:nowrap;
  background:var(--surface-2);border:1px solid var(--border);color:var(--text-2)}
.pill[data-on="1"]{color:var(--ok);border-color:color-mix(in srgb,var(--ok) 40%,transparent)}
.pill[data-on="0"]{color:#dc2626;border-color:rgba(220,38,38,.4)}
.card{background:var(--surface);border:1px solid var(--border);border-radius:12px;
  padding:14px 16px;margin-bottom:12px}
.hint{margin:6px 0 10px;color:var(--muted);font-size:12px}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
pre.code{margin:0;padding:12px;border-radius:10px;background:var(--surface-2);
  border:1px solid var(--border);overflow:auto;font-size:12px;line-height:1.55;
  font-family:ui-monospace,SFMono-Regular,Menlo,monospace;max-height:260px}
.row{display:flex;gap:8px;align-items:center;margin-top:10px;flex-wrap:wrap}
.btn{font:inherit;font-size:12.5px;padding:6px 12px;border-radius:8px;cursor:pointer;
  border:1px solid var(--border);background:var(--surface-2);color:var(--text);text-decoration:none}
.btn:hover{border-color:var(--accent)}
.btn.ghost{background:transparent}
.msg{font-size:12px;color:var(--muted)}
.msg[data-c="ok"]{color:var(--ok)} .msg[data-c="err"]{color:#dc2626}
.models{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.mchip{font-family:ui-monospace,monospace;font-size:11.5px;padding:3px 8px;border-radius:7px;
  background:var(--surface-2);border:1px solid var(--border)}
summary{cursor:pointer;font-size:13px;font-weight:600;color:var(--text-2);
  text-transform:uppercase;letter-spacing:.06em}
summary:hover{color:var(--text)}
`

const pageJS = `
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };

  function esc(v) {
    return String(v == null ? "" : v).replace(/&/g, "&amp;").replace(/</g, "&lt;")
      .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  function fail(text) {
    $("state").textContent = "error";
    $("state").dataset.on = "0";
    $("models").innerHTML = "";
    $("models-hint").textContent = text;
  }

  // The state is served from this page's own resource route, which needs no
  // management key. Check the status: a 401 body is valid JSON, and rendering
  // it as config is how this page used to report zeros and an error as its
  // settings.
  function get(url) {
    return fetch(url).then(function (r) {
      return r.text().then(function (t) {
        if (!r.ok) throw new Error(t && t.length < 200 ? t : "HTTP " + r.status);
        return t;
      });
    });
  }

  get("?asset=state")
    .then(function (t) {
      var cfg = JSON.parse(t);
      var on = cfg.enabled !== false;
      $("state").textContent = on ? "active" : "disabled";
      $("state").dataset.on = on ? "1" : "0";
      var models = cfg.models || [];
      $("models").innerHTML = models.map(function (name) {
        return '<span class="mchip">' + esc(name) + "</span>";
      }).join("");
      $("models-hint").textContent = models.length
        ? models.length + " models. Some are served intermittently upstream."
        : "No models configured.";
    })
    .catch(function (e) { fail("Could not read the plugin config: " + e.message); });

  get("?asset=provider").then(function (t) { $("provider").textContent = t; })
    .catch(function (e) { $("provider").textContent = "unavailable: " + e.message; });

  $("copy-provider").addEventListener("click", function () {
    var text = $("provider").textContent;
    var out = $("msg");
    (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject())
      .then(function () { out.textContent = "Copied."; out.dataset.c = "ok"; })
      .catch(function () { out.textContent = "Clipboard blocked — select the text instead."; out.dataset.c = "err"; });
    setTimeout(function () { out.textContent = ""; }, 2500);
  });
})();
`
