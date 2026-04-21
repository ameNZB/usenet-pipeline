package services

import (
	"html/template"

	"github.com/ameNZB/usenet-pipeline/storage"
)

// The local UI uses one layout template with a sidebar + top bar, and each
// page is a content block composed into it. Keeping all templates inline
// avoids a static-files mount and makes the single-binary deploy story
// work unchanged. When this file grows past ~1000 lines we'll migrate to
// embed.FS with separate .html files.
//
// Two client-side libraries are loaded from CDN:
//   htmx    — drives the forms + future partial-swap interactions.
//   alpine  — small amount of local component state (menus, dialogs).
// Both are loaded at the bottom of the layout so page content renders
// immediately even if the CDN is slow.

const layoutHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>{{block "title" .}}Agent{{end}} — indexer-agent</title>
<link rel="icon" href="data:,"> {{/* suppress favicon 404 without shipping one */}}
<style>
:root {
  --bg:         #0f1115;
  --sidebar-bg: #151820;
  --panel-bg:   #1a1e28;
  --panel-edge: #252a36;
  --text:       #e6e8ee;
  --text-muted: #8b8f9a;
  --accent:     #4a9eff;
  --accent-dim: #2b5a8f;
  --ok:         #10b981;
  --warn:       #f59e0b;
  --err:        #ef4444;
  --row-hover:  #1d2230;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
  background: var(--bg);
  color: var(--text);
  display: flex;
  min-height: 100vh;
}

/* ── Sidebar ──────────────────────────────────────────────── */
.sidebar {
  width: 260px;
  min-width: 260px;
  background: var(--sidebar-bg);
  border-right: 1px solid var(--panel-edge);
  display: flex;
  flex-direction: column;
  padding: 16px;
  gap: 16px;
  position: sticky;
  top: 0;
  height: 100vh;
  overflow-y: auto;
}
.brand {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-weight: 600;
  font-size: 15px;
  letter-spacing: 0.02em;
  color: var(--text);
}
.brand .version { color: var(--text-muted); font-weight: 400; font-size: 11px; }

.card {
  background: var(--panel-bg);
  border: 1px solid var(--panel-edge);
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 12.5px;
}
.card-head {
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.08em;
  font-size: 10.5px;
  font-weight: 600;
  margin-bottom: 8px;
}
.status-row { display: flex; justify-content: space-between; padding: 2px 0; }
.status-row .k { color: var(--text-muted); }
.status-row .v { color: var(--text); font-variant-numeric: tabular-nums; }

.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; vertical-align: middle; background: var(--text-muted); }
.dot.ok  { background: var(--ok);  box-shadow: 0 0 6px rgba(16,185,129,0.5); }
.dot.err { background: var(--err); box-shadow: 0 0 6px rgba(239,68,68,0.5); }
.dot.warn{ background: var(--warn);box-shadow: 0 0 6px rgba(245,158,11,0.5); }

.speed-row { display: flex; justify-content: space-between; align-items: baseline; }
.speed-row .down { color: var(--ok); }
.speed-row .up   { color: var(--accent); }
.speed-row .num  { font-weight: 600; font-variant-numeric: tabular-nums; }
.speed-row .unit { color: var(--text-muted); font-size: 11px; margin-left: 2px; }

#speed-graph {
  width: 100%; height: 40px;
  margin-top: 6px;
  stroke: var(--accent); fill: none; stroke-width: 1;
}

.nav-group { display: flex; flex-direction: column; }
.nav-group .head {
  color: var(--text-muted); text-transform: uppercase;
  letter-spacing: 0.08em; font-size: 10.5px; font-weight: 600;
  padding: 4px 8px; margin-top: 4px;
}
.nav-group a {
  display: flex; justify-content: space-between; align-items: center;
  padding: 7px 10px; color: var(--text-muted); text-decoration: none;
  border-radius: 4px; font-size: 13px;
}
.nav-group a:hover { color: var(--text); background: var(--row-hover); }
.nav-group a.active { color: var(--text); background: var(--accent-dim); }
.nav-group a .count {
  color: var(--text);
  background: var(--accent-dim);
  font-size: 10.5px;
  padding: 1px 6px;
  border-radius: 8px;
  min-width: 16px;
  text-align: center;
}
.nav-group a .count:empty { display: none; }

.sidebar .footer {
  margin-top: auto;
  color: var(--text-muted);
  font-size: 11px;
  padding-top: 8px;
  border-top: 1px solid var(--panel-edge);
}

/* ── Main column ──────────────────────────────────────────── */
.main {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.topbar {
  height: 48px;
  border-bottom: 1px solid var(--panel-edge);
  display: flex;
  align-items: center;
  padding: 0 20px;
  background: var(--sidebar-bg);
  gap: 16px;
  position: sticky; top: 0; z-index: 10;
}
.topbar h1 { margin: 0; font-size: 15px; font-weight: 600; }
.topbar .spacer { flex: 1; }
.topbar .btn { padding: 5px 12px; }

.content { padding: 20px; flex: 1; }
.content p.lead { color: var(--text-muted); margin-top: 0; }

/* ── Controls ─────────────────────────────────────────────── */
input, textarea, select, button {
  background: var(--panel-bg);
  color: var(--text);
  border: 1px solid var(--panel-edge);
  border-radius: 4px;
  padding: 5px 9px;
  font: inherit;
}
input:focus, textarea:focus, select:focus { outline: none; border-color: var(--accent); }
textarea { min-height: 4em; width: 100%; resize: vertical; }
button { cursor: pointer; }
button:hover { border-color: var(--accent); }
button.primary  { background: var(--accent-dim); border-color: var(--accent); }
button.primary:hover { background: var(--accent); color: #fff; }
button.danger   { background: #3a1f1f; border-color: #6a2c2c; }
button.danger:hover { background: #5a2828; }

table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { padding: 8px 10px; border-bottom: 1px solid var(--panel-edge); text-align: left; vertical-align: top; }
th { color: var(--text-muted); font-weight: 500; text-transform: uppercase; font-size: 10.5px; letter-spacing: 0.06em; }
tr:hover td { background: var(--row-hover); }

code { background: var(--panel-bg); padding: 1px 5px; border-radius: 3px; font-size: 12px; }

.badge { display: inline-block; padding: 2px 7px; font-size: 10.5px; border-radius: 10px; background: var(--panel-edge); color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.04em; }
.badge.ok      { background: rgba(16,185,129,0.15);  color: var(--ok);  }
.badge.err     { background: rgba(239,68,68,0.15);   color: var(--err); }
.badge.warn    { background: rgba(245,158,11,0.15);  color: var(--warn);}
.badge.yml     { background: rgba(74,158,255,0.15);  color: var(--accent);}
.badge.env     { background: rgba(245,158,11,0.12);  color: #d2a35a;    }
.badge.local   { background: rgba(16,185,129,0.1);   color: #4db391;    }
.badge.site    { background: rgba(74,158,255,0.12);  color: #6aaeff;    }
.badge.default { background: var(--panel-edge);      color: var(--text-muted); }

.flash { padding: 8px 12px; border-radius: 4px; margin-bottom: 16px; font-size: 13px; }
.flash.ok  { background: rgba(16,185,129,0.1); border: 1px solid rgba(16,185,129,0.4); color: #4db391; }
.flash.err { background: rgba(239,68,68,0.1);  border: 1px solid rgba(239,68,68,0.4);  color: #ee8080; }

.small { color: var(--text-muted); font-size: 11.5px; }

.row-form { display: contents; } /* forms in table rows don't disrupt layout */
</style>
</head>
<body>
<aside class="sidebar">
  <div class="brand">
    <span>indexer-agent</span>
    <span class="version">local UI</span>
  </div>

  <div class="card">
    <div class="card-head">Agent</div>
    <div class="status-row"><span class="k"><span class="dot ok" id="live-agent-dot"></span>State</span><span class="v" id="live-phase">idle</span></div>
    <div class="status-row"><span class="k">VPN</span><span class="v"><span class="dot {{if eq .VPNStatus "Connected"}}ok{{else if eq .VPNStatus "Disconnected"}}err{{else}}warn{{end}}" id="live-vpn-dot"></span><span id="live-vpn">{{.VPNStatus}}</span></span></div>
    <div class="status-row"><span class="k">Public IP</span><span class="v" id="live-ip">{{.PublicIP}}</span></div>
  </div>

  <div class="card">
    <div class="card-head">Throughput</div>
    <div class="speed-row">
      <span>↓ <span class="down num" id="live-down">0</span><span class="unit" id="live-down-unit">B/s</span></span>
      <span>↑ <span class="up num" id="live-up">0</span><span class="unit" id="live-up-unit">B/s</span></span>
    </div>
    <svg id="speed-graph" viewBox="0 0 100 30" preserveAspectRatio="none"></svg>
  </div>

  <div class="card">
    <div class="card-head">Disk</div>
    <div class="status-row"><span class="k">Free</span><span class="v" id="live-disk-free">—</span></div>
    <div class="status-row"><span class="k">Reserved</span><span class="v" id="live-disk-reserved">—</span></div>
  </div>

  <div class="nav-group">
    <div class="head">Navigate</div>
    <a href="/jobs"{{if eq .CurrentPage "jobs"}} class="active"{{end}}>Jobs<span class="count" id="nav-jobs-count"></span></a>
    <a href="/watches"{{if eq .CurrentPage "watches"}} class="active"{{end}}>Watch Folders</a>
    <a href="/groups"{{if eq .CurrentPage "groups"}} class="active"{{end}}>Groups</a>
    <a href="/"{{if eq .CurrentPage "config"}} class="active"{{end}}>Config</a>
  </div>

  <div class="footer">Loopback only — bind: <code>{{.BindAddr}}</code></div>
</aside>

<div class="main">
  <header class="topbar">
    <h1>{{template "title" .}}</h1>
    <div class="spacer"></div>
    {{block "topbar-actions" .}}{{end}}
  </header>
  <main class="content">
    {{if .Flash}}<div class="flash ok">{{.Flash}}</div>{{end}}
    {{if .Err}}<div class="flash err">{{.Err}}</div>{{end}}
    {{block "content" .}}<p>no content</p>{{end}}
  </main>
</div>

<script src="https://unpkg.com/htmx.org@2.0.4" defer></script>
<script src="https://unpkg.com/alpinejs@3.14.9/dist/cdn.min.js" defer></script>
<script>
// SSE client for the sidebar live status. One EventSource per tab,
// reconnects automatically via the browser's built-in EventSource retry.
// Kept tiny + framework-free because the sidebar is on every page and
// loading Alpine/HTMX would be overkill for "set 8 text nodes."
(function() {
  const MAX_POINTS = 60; // ~90s at the 1.5s SSE cadence
  const dl = [], ul = [];
  const graph = document.getElementById('speed-graph');

  // fmtSpeed takes MB/s and returns [number, unit] tuned for readability:
  // big speeds in MB/s, smaller in KB/s, idle in B/s. No floating "0.02"
  // MB/s that requires squinting at the page.
  function fmtSpeed(mbps) {
    if (!mbps || mbps < 0) return ['0', 'B/s'];
    if (mbps >= 1) return [mbps.toFixed(2), 'MB/s'];
    const kbs = mbps * 1024;
    if (kbs >= 1) return [kbs.toFixed(0), 'KB/s'];
    return [Math.round(mbps * 1024 * 1024).toString(), 'B/s'];
  }

  function setText(id, txt) {
    const el = document.getElementById(id);
    if (el && el.textContent !== String(txt)) el.textContent = txt;
  }
  function setDot(id, cls) {
    const el = document.getElementById(id);
    if (!el) return;
    el.className = 'dot ' + cls;
  }

  // redrawGraph draws download + upload as two polylines, scaled to the
  // rolling window's own peak so a low-activity period still renders as
  // something visible rather than a flat line on the x-axis.
  const svgNS = 'http://www.w3.org/2000/svg';
  function redrawGraph() {
    if (!graph) return;
    while (graph.firstChild) graph.removeChild(graph.firstChild);
    if (dl.length < 2) return;
    let peak = 0;
    for (const v of dl) if (v > peak) peak = v;
    for (const v of ul) if (v > peak) peak = v;
    if (peak < 0.05) peak = 0.05;
    const drawLine = (arr, stroke) => {
      const pts = arr.map((v, i) => {
        const x = (i / (MAX_POINTS - 1)) * 100;
        const y = 30 - (v / peak) * 28;
        return x.toFixed(1) + ',' + y.toFixed(1);
      }).join(' ');
      const p = document.createElementNS(svgNS, 'polyline');
      p.setAttribute('points', pts);
      p.setAttribute('stroke', stroke);
      p.setAttribute('fill', 'none');
      p.setAttribute('stroke-width', '1.2');
      graph.appendChild(p);
    };
    drawLine(dl, '#10b981');
    // Only draw the upload line if there's something to draw — avoids
    // a distracting flat line pinned to the x-axis during pure downloads.
    if (ul.some(v => v > 0)) drawLine(ul, '#4a9eff');
  }

  function onStatus(d) {
    const [dv, du] = fmtSpeed(d.download_mbps);
    const [uv, uu] = fmtSpeed(d.upload_mbps);
    setText('live-down', dv); setText('live-down-unit', du);
    setText('live-up', uv);   setText('live-up-unit', uu);
    setText('live-phase', d.phase || 'idle');
    setText('live-vpn', d.vpn_status || 'Unknown');
    setText('live-ip', d.public_ip || '—');

    // Agent "State" dot green when doing useful work, amber when idle —
    // lets the operator see at a glance whether the pipeline is flowing.
    setDot('live-agent-dot', d.phase && d.phase !== 'idle' ? 'ok' : 'warn');
    setDot('live-vpn-dot',
      d.vpn_status === 'Connected' ? 'ok' :
      d.vpn_status === 'Disconnected' ? 'err' : 'warn');

    if (typeof d.disk_free_gb === 'number') {
      setText('live-disk-free', d.disk_free_gb.toFixed(1) + ' GB');
    }
    if (typeof d.disk_reserved_gb === 'number') {
      setText('live-disk-reserved', d.disk_reserved_gb.toFixed(1) + ' GB');
    }

    if (d.jobs) {
      // Active = queued + processing; surfaces work-in-flight on the nav
      // label. Completed/failed history doesn't belong next to the link.
      const active = (d.jobs.queued || 0) + (d.jobs.processing || 0);
      setText('nav-jobs-count', active > 0 ? active : '');
    }

    dl.push(d.download_mbps || 0);
    ul.push(d.upload_mbps || 0);
    if (dl.length > MAX_POINTS) dl.shift();
    if (ul.length > MAX_POINTS) ul.shift();
    redrawGraph();
  }

  try {
    const es = new EventSource('/events');
    es.addEventListener('status', ev => {
      try { onStatus(JSON.parse(ev.data)); } catch (e) {}
    });
    // EventSource auto-reconnects on network errors; we only flag it
    // visually so the operator knows the sidebar isn't lying.
    es.addEventListener('error', () => setDot('live-agent-dot', 'warn'));
  } catch (e) { /* old browser / CSP; sidebar stays static */ }
})();
</script>
</body>
</html>`

// buildPageTemplate composes the shared layout with a page-specific
// content block. Each page calls this once at package init so parse
// errors surface on boot rather than on first request.
func buildPageTemplate(name, src string, funcs template.FuncMap) *template.Template {
	t := template.New(name)
	if funcs != nil {
		t = t.Funcs(funcs)
	}
	return template.Must(template.Must(t.Parse(layoutHTML)).Parse(src))
}

// baseData fills in the fields the layout needs. Every handler wraps
// its page-specific data with this so sidebar/status rendering doesn't
// require each handler to remember the full field set.
func (u *LocalUI) baseData(currentPage string, flash, errMsg string) map[string]any {
	vpn := "Unknown"
	ip := "—"
	if storage.GlobalState.VPNStatus != "" {
		storage.GlobalState.RLock()
		vpn = storage.GlobalState.VPNStatus
		if storage.GlobalState.PublicIP != "" {
			ip = storage.GlobalState.PublicIP
		}
		storage.GlobalState.RUnlock()
	}
	return map[string]any{
		"CurrentPage": currentPage,
		"Flash":       flash,
		"Err":         errMsg,
		"BindAddr":    u.bindAddr,
		"VPNStatus":   vpn,
		"PublicIP":    ip,
	}
}
