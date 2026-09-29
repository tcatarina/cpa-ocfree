package main

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
      <p class="sub">Satisfies the free-tier request contract on the way upstream.</p>
    </div>
    <span id="state" class="pill">loading</span>
  </header>

  <section class="stats">
    <div class="tile"><span class="n" id="n-models">0</span><span class="l">models</span></div>
    <div class="tile"><span class="n" id="n-tools">0</span><span class="l">tool names</span></div>
    <div class="tile"><span class="n" id="n-ua">0</span><span class="l">ua version</span></div>
  </section>

  <section class="card">
    <h2>Provider block</h2>
    <p class="hint">Paste into your CPA <code>config.yaml</code>. The keyless pool entries
    give the rotation independent auth records; the plugin handles the rest.</p>
    <pre id="provider" class="code">…</pre>
    <div class="row">
      <button class="btn" id="copy-provider">Copy</button>
      <a class="btn ghost" href="?asset=config" download="ocfree.yaml">Download plugin config</a>
    </div>
  </section>

  <section class="card">
    <h2>Contract</h2>
    <div id="contract" class="contract"></div>
  </section>

  <section class="card">
    <h2>Models</h2>
    <div id="models" class="models"></div>
    <p class="hint" id="models-hint"></p>
  </section>

  <section class="card">
    <h2>Edit</h2>
    <p class="hint">Tool names and models are config, not code. An upstream change is an edit here.</p>
    <textarea id="editor" class="editor" spellcheck="false"></textarea>
    <div class="row">
      <button class="btn" id="save">Save</button>
      <button class="btn ghost" id="reload">Reload</button>
      <span id="msg" class="msg"></span>
    </div>
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
.sub{margin:4px 0 0;color:var(--muted);font-size:13px}
.pill{font-size:11px;padding:3px 9px;border-radius:999px;
  background:var(--surface-2);border:1px solid var(--border);color:var(--text-2)}
.pill[data-on="1"]{color:var(--ok);border-color:color-mix(in srgb,var(--ok) 40%,transparent)}
.stats{display:grid;grid-template-columns:repeat(3,1fr);gap:10px;margin-bottom:16px}
.tile{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:12px 14px;
  display:flex;flex-direction:column;gap:2px}
.tile .n{font-size:20px;font-weight:640;font-variant-numeric:tabular-nums}
.tile .l{font-size:11px;color:var(--muted);text-transform:uppercase;letter-spacing:.06em}
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
.contract{display:grid;gap:6px;margin-top:8px}
.crow{display:flex;gap:10px;align-items:baseline;font-size:12.5px}
.crow .k{font-family:ui-monospace,monospace;color:var(--accent);white-space:nowrap}
.crow .v{color:var(--text-2)}
.models{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.mchip{font-family:ui-monospace,monospace;font-size:11.5px;padding:3px 8px;border-radius:7px;
  background:var(--surface-2);border:1px solid var(--border)}
.mchip[data-free="0"]{opacity:.55}
.editor{width:100%;min-height:220px;font-family:ui-monospace,monospace;font-size:12px;
  padding:10px;border-radius:10px;background:var(--surface-2);color:var(--text);
  border:1px solid var(--border);resize:vertical}
`

const pageJS = `
(function () {
  "use strict";
  var API = "__BASE__";
  var $ = function (id) { return document.getElementById(id); };
  function esc(v) {
    return String(v == null ? "" : v).replace(/&/g, "&amp;").replace(/</g, "&lt;")
      .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }
  function msg(text, kind) {
    var el = $("msg"); el.textContent = text; el.dataset.c = kind || "";
    if (text) setTimeout(function () { if (el.textContent === text) el.textContent = ""; }, 4000);
  }
  function setMsg(text, kind) { msg(text, kind); }

  function toYAML(v, indent) {
    var pad = new Array(indent + 1).join(" ");
    if (v === null || v === undefined) return "null";
    if (Array.isArray(v)) {
      if (!v.length) return "[]";
      return "\n" + v.map(function (item) {
        return pad + "- " + toYAML(item, indent + 2);
      }).join("\n");
    }
    if (typeof v === "object") {
      var keys = Object.keys(v);
      if (!keys.length) return "{}";
      return "\n" + keys.map(function (k) {
        var val = v[k];
        if (val && typeof val === "object") return pad + k + ":" + toYAML(val, indent + 2);
        return pad + k + ": " + scalar(val);
      }).join("\n");
    }
    return scalar(v);
  }
  function scalar(v) {
    if (typeof v === "string") {
      if (v === "" || /[:#\-\n]/.test(v) || /^\s|\s$/.test(v)) return JSON.stringify(v);
      return v;
    }
    return String(v);
  }

  function render(cfg) {
    var on = cfg.enabled !== false;
    var pill = $("state");
    pill.textContent = on ? "active" : "disabled";
    pill.dataset.on = on ? "1" : "0";
    $("n-models").textContent = (cfg.models || []).length;
    var tools = cfg.tools || {};
    var n = (cfg.tool_names || []).length;
    Object.keys(tools).forEach(function (k) { n += (tools[k] || []).length ? 0 : 0; });
    $("n-tools").textContent = n;
    var m = /opencode\/v?(\d+)\.(\d+)/.exec(cfg.user_agent || "opencode/1.18.31");
    $("n-ua").textContent = m ? m[1] + "." + m[2] : "?";

    $("contract").innerHTML = [
      ["stream", "forced true on the upstream call"],
      ["tools", "seeded with the client tool names when the caller sends none"],
      ["x-opencode-session", "deterministic per conversation, so the prompt cache stays warm"],
      ["x-opencode-request", "regenerated per attempt"],
      ["User-Agent", (cfg.user_agent || "opencode/1.18.31") + " — also clears the edge bot check"]
    ].map(function (row) {
      return '<div class="crow"><span class="k">' + esc(row[0]) + '</span><span class="v">' + esc(row[1]) + "</span></div>";
    }).join("");

    $("models").innerHTML = (cfg.models || []).map(function (name) {
      return '<span class="mchip" data-free="' + (/-free$/.test(name) ? "1" : "0") + '">' + esc(name) + "</span>";
    }).join("");
    $("models-hint").textContent =
      "Dimmed entries have no -free suffix. The suffix is not a reliable filter: the live";
    $("editor").value = toYAML(cfg, 0).replace(/^\n/, "");
  }

  function load() {
    return fetch(API).then(function (r) { return r.json(); }).then(render);
  }

  load();
  fetch("?asset=provider").then(function (r) { return r.text(); })
    .then(function (t) { $("provider").textContent = t; });

  $("reload").addEventListener("click", function () {
    load().then(function () { setMsg("Reloaded.", "ok"); })
      .catch(function (e) { setMsg("Reload failed: " + e.message, "err"); });
  });

  $("copy-provider").addEventListener("click", function () {
    var text = $("provider").textContent;
    (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject())
      .then(function () { setMsg("Copied provider block.", "ok"); })
      .catch(function () { setMsg("Clipboard blocked — select the text instead.", "err"); });
  });

  $("save").addEventListener("click", function () {
    fetch(API, {
      method: "PUT",
      headers: { "Content-Type": "application/yaml" },
      body: $("editor").value
    }).then(function (r) {
      return r.json().then(function (d) {
        if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
        return d;
      });
    }).then(function (cfg) {
      render(cfg); setMsg("Saved.", "ok");
    }).catch(function (e) { setMsg("Save failed: " + e.message, "err"); });
  });
})();
`
