package services

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ameNZB/usenet-pipeline/client"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
)

// LocalUI serves a small HTML + JSON interface on loopback for users who
// prefer editing the agent's on-disk config (and the per-tracker passkeys)
// from a browser instead of SSH'ing in. Default bind is 127.0.0.1 so it's
// invisible outside the host unless the operator sets LOCAL_UI_BIND.
//
// It is deliberately feature-narrow:
//   - GET  /         → edit config.yml + secrets.yml
//   - POST /config   → write config.yml via layered.WriteYml
//   - POST /secrets  → write secrets.yml via SecretsStore.Set
//   - GET  /status   → current Layered + secrets snapshot for polling
//
// Anything that would need cross-host access (dashboard, job history) is
// served by the main site. This service never accepts site traffic.
type LocalUI struct {
	cfg      *config.Config
	secrets  *SecretsStore
	db       *storage.DB
	port     int
	bindAddr string

	// site is the authenticated client to the indexer site. Optional:
	// the local UI works without it (groups, watch folders, local
	// config), but the "Agent settings" panel's write path requires
	// it to PUT /api/agent/web-config. Wired via SetSite after
	// StartLocalUI returns because the site client is constructed in
	// main.go alongside poll/status/complete and the LocalUI is
	// created before them.
	site *client.SiteClient
}

// SetSite wires the site client into the local UI. Called from main
// after client.New so the /web-override form handler can write back
// to the site. Concurrent writes are fine — the field is only read
// from HTTP handler goroutines that start after StartLocalUI returns.
func (u *LocalUI) SetSite(c *client.SiteClient) { u.site = c }

func StartLocalUI(cfg *config.Config, secrets *SecretsStore, db *storage.DB) *LocalUI {
	port, _ := strconv.Atoi(os.Getenv("LOCAL_UI_PORT"))
	if port <= 0 {
		return nil
	}
	bind := os.Getenv("LOCAL_UI_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	ui := &LocalUI{cfg: cfg, secrets: secrets, db: db, port: port, bindAddr: bind}
	addr := net.JoinHostPort(bind, strconv.Itoa(port))

	mux := http.NewServeMux()
	// Shared assets — design tokens vendored from the site. Kept
	// under /_shared/ to flag them visually as "same file as the
	// site ships" and to reserve that prefix for future shared
	// components (a component library, icons, etc).
	mux.HandleFunc("/_shared/tokens.css", ServeTokensCSS)
	mux.HandleFunc("/_shared/components.css", ServeComponentsCSS)
	mux.HandleFunc("/_shared/agent-shell.css", ServeAgentShellCSS)
	// Agent-only assets. Everything under /static/ is baked into
	// the binary via go:embed and cached aggressively by the
	// browser — see cssHandler in localui_assets.go.
	mux.HandleFunc("/static/localui.css", ServeLocalUICSS)

	mux.HandleFunc("/", ui.handleIndex)
	mux.HandleFunc("/config", ui.handleConfig)
	mux.HandleFunc("/config/web-override", ui.handleWebOverride)
	mux.HandleFunc("/secrets", ui.handleSecrets)
	mux.HandleFunc("/status", ui.handleStatus)
	mux.HandleFunc("/groups", ui.handleGroups)
	mux.HandleFunc("/groups/create", ui.handleGroupCreate)
	mux.HandleFunc("/groups/update", ui.handleGroupUpdate)
	mux.HandleFunc("/groups/delete", ui.handleGroupDelete)
	mux.HandleFunc("/watches", ui.handleWatches)
	mux.HandleFunc("/watches/create", ui.handleWatchCreate)
	mux.HandleFunc("/watches/update", ui.handleWatchUpdate)
	mux.HandleFunc("/watches/delete", ui.handleWatchDelete)
	mux.HandleFunc("/jobs", ui.handleJobs)
	mux.HandleFunc("/jobs/retry", ui.handleJobRetry)
	mux.HandleFunc("/jobs/delete", ui.handleJobDelete)
	mux.HandleFunc("/events", ui.handleEvents)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("Local UI listening on http://%s/", addr)
		if bind != "127.0.0.1" && bind != "localhost" {
			log.Printf("WARNING: Local UI bound to %s — anyone on that network can edit config/secrets. Use 127.0.0.1 unless you've put this behind auth.", bind)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Local UI server exited: %v", err)
		}
	}()
	return ui
}

// URL is the URL the site UI shows to link back here. When bound to
// loopback we still report it so the user sees where to reach it from
// the agent host; remote users will have to SSH-tunnel.
func (u *LocalUI) URL() string {
	if u == nil {
		return ""
	}
	host := u.bindAddr
	if host == "0.0.0.0" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d/", host, u.port)
}

func (u *LocalUI) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type kv struct {
		Key, Value, Source string
	}
	rows := make([]kv, 0, len(config.ConfigYmlKeys))
	for _, k := range config.ConfigYmlKeys {
		rows = append(rows, kv{Key: k, Value: u.cfg.Layered.Effective(k), Source: sourceFor(u.cfg.Layered, k)})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"config":          rows,
		"config_writable": u.cfg.Layered.Writable(),
		"secrets_hosts":   u.secrets.List(),
		"has_secrets":     u.secrets.Has(),
	})
}

// sourceFor returns the tier that supplied a key's effective value so the
// local UI can show a small badge matching the site's state model.
func sourceFor(l *config.Layered, key string) string {
	snap := l.LocalSnapshot()
	if v, ok := snap[key]; ok {
		return v.Source
	}
	return "default"
}

func (u *LocalUI) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updates := map[string]string{}
	for _, k := range config.ConfigYmlKeys {
		if v := r.PostForm.Get(k); v != "" || r.PostForm.Has(k) {
			updates[k] = strings.TrimSpace(v)
		}
	}
	written, err := u.cfg.Layered.WriteYml(updates)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.cfg.Refresh()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "written": written})
}

// handleWebOverride writes (or clears) a single web-tier override on
// the site via PUT /api/agent/web-config. The form posts three fields:
// "key", "value" (empty to clear), and "return_to" (redirect target).
// This is the agent's first "manage the site from the local UI" entry
// point — everything else the site's admin dashboard does still lives
// there, but the per-agent runtime knobs can now be tweaked without
// alt-tabbing.
func (u *LocalUI) handleWebOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if u.site == nil {
		http.Error(w, "site client not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostForm.Get("key"))
	value := strings.TrimSpace(r.PostForm.Get("value"))
	returnTo := r.PostForm.Get("return_to")
	if returnTo == "" {
		returnTo = "/"
	}
	if key == "" {
		redirectWithFlash(w, r, returnTo, "", "key is required")
		return
	}
	if err := u.site.PutWebConfig(key, value); err != nil {
		redirectWithFlash(w, r, returnTo, "", err.Error())
		return
	}
	// Re-fetch the effective config from the site so the next render
	// reflects what we just wrote (avoids a confusing lag where the
	// form shows the old value until the next poll-driven refresh).
	if rc, err := u.site.GetConfig(); err == nil && rc != nil {
		u.cfg.Layered.ApplyWeb(rc.WebOverrides)
	}
	msg := "set " + key
	if value == "" {
		msg = "cleared " + key
	}
	redirectWithFlash(w, r, returnTo, msg, "")
}

func (u *LocalUI) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host := r.PostForm.Get("host")
	key := r.PostForm.Get("key")
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	if err := u.secrets.Set(host, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ── Template FuncMaps ─────────────────────────────────────────────────────

// groupsTmplFuncs: small helpers the groups form relies on. Registered
// before Parse so the template compiler treats them as function calls
// instead of field lookups (which would fail at render time).
var groupsTmplFuncs = template.FuncMap{
	// derefBool reads through a *bool, returning false for nil so template
	// comparisons like `{{if derefBool .Obfuscate}}` don't panic on the
	// "inherit global" case.
	"derefBool": func(b *bool) bool {
		if b == nil {
			return false
		}
		return *b
	},
}

// jobsTmplFuncs: helpers for the /jobs page.
var jobsTmplFuncs = template.FuncMap{
	"formatTime": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "—"
		}
		return t.Local().Format("Jan 02 15:04:05")
	},
	// statusClass maps a job status to a CSS class so the status badge
	// can be coloured without the template having to branch five times.
	"statusClass": func(s string) string {
		switch s {
		case "completed":
			return "ok"
		case "failed":
			return "err"
		case "processing":
			return "yml"
		case "cancelled":
			return "default"
		default: // queued
			return "env"
		}
	},
}

// watchesTmplFuncs: helpers for comparing nullable group FKs in the
// watches template.
var watchesTmplFuncs = template.FuncMap{
	// isGroup reports whether a nullable group pointer matches a given id.
	// Lets the <select> option loop mark the currently-assigned group
	// without template gymnastics over *int64.
	"isGroup": func(sel *int64, id int64) bool {
		return sel != nil && *sel == id
	},
}

// ── Page templates ────────────────────────────────────────────────────────
//
// Each page overrides two blocks defined by the layout: "title" (shown in
// <title> + top bar <h1>) and "content" (main column). The shared sidebar,
// status panel, and Flash/Err rendering come from the layout — page
// templates focus only on what's actually different between pages.

var groupsTmpl = buildPageTemplate("groups", `
{{define "title"}}Groups{{end}}
{{define "content"}}
<p class="lead">
Groups define where offline uploads post (newsgroups) and let you override
the global PAR2/screenshot/obfuscation defaults per category. Leave a
field blank to inherit the global default.
</p>

<h2>Existing</h2>
{{if .Groups}}
<table>
<tr>
  <th>Name</th><th>Type</th><th>Newsgroups</th>
  <th>Samples</th><th>Sec/sample</th>
  <th>Banned ext.</th>
  <th>PAR2%</th><th>Obf.</th><th>Watermark</th>
  <th>Source</th><th>v</th><th></th>
</tr>
{{range .Groups}}
<tr>
<form class="row-form" method="post" action="/groups/update">
  <input type="hidden" name="id" value="{{.ID}}">
  <td><input name="name" value="{{.Name}}" size="12" required{{if eq .Source "site"}} readonly{{end}}></td>
  <td><input name="type" value="{{.Type}}" size="7" placeholder="video" list="type-list"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><textarea name="newsgroups" rows="2" required{{if eq .Source "site"}} readonly{{end}}>{{range .Newsgroups}}{{.}}
{{end}}</textarea></td>
  <td><input name="screenshots" value="{{if .Screenshots}}{{.Screenshots}}{{end}}" size="3" type="number" min="0"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><input name="sample_seconds" value="{{if .SampleSeconds}}{{.SampleSeconds}}{{end}}" size="3" type="number" min="1"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><textarea name="banned_extensions" rows="2" cols="12" placeholder="(default list)"{{if eq .Source "site"}} readonly{{end}}>{{range .BannedExtensions}}{{.}}
{{end}}</textarea></td>
  <td><input name="par2_redundancy" value="{{if .Par2Redundancy}}{{.Par2Redundancy}}{{end}}" size="3" type="number" min="0" max="100"{{if eq .Source "site"}} readonly{{end}}></td>
  <td>
    <select name="obfuscate"{{if eq .Source "site"}} disabled{{end}}>
      <option value=""{{if not .Obfuscate}} selected{{end}}>inherit</option>
      {{if .Obfuscate}}
        {{if derefBool .Obfuscate}}<option value="1" selected>yes</option><option value="0">no</option>
        {{else}}<option value="1">yes</option><option value="0" selected>no</option>{{end}}
      {{else}}<option value="1">yes</option><option value="0">no</option>{{end}}
    </select>
  </td>
  <td><input name="watermark_text" value="{{.WatermarkText}}" size="10" placeholder="-YourTag"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><span class="badge {{.Source}}">{{.Source}}</span></td>
  <td class="small">{{.Version}}</td>
  <td>
    {{if ne .Source "site"}}<button type="submit" class="primary">Save</button>{{end}}
</form>
{{if ne .Source "site"}}
<form class="row-form" method="post" action="/groups/delete" style="display:inline;" onsubmit="return confirm('Delete {{.Name}}?')">
  <input type="hidden" name="id" value="{{.ID}}">
  <button type="submit" class="danger">Delete</button>
</form>
{{end}}
  </td>
</tr>
{{end}}
</table>
<datalist id="type-list">
  <option value="video"><option value="manga"><option value="music">
</datalist>
{{else}}
<p class="small">No groups defined yet.</p>
{{end}}

<h2 style="margin-top:28px;">Create new</h2>
<form method="post" action="/groups/create">
<table>
<tr><th>Name</th><td><input name="name" placeholder="anime" required></td></tr>
<tr><th>Type</th><td><input name="type" placeholder="video" list="type-list-new"><datalist id="type-list-new"><option value="video"><option value="manga"><option value="music"></datalist> <span class="small">video / manga / music — or any other label for a custom sampling behaviour</span></td></tr>
<tr><th>Newsgroups</th><td><textarea name="newsgroups" rows="3" placeholder="alt.binaries.multimedia.anime.highspeed&#10;alt.binaries.boneless" required></textarea><div class="small">One per line (commas also work).</div></td></tr>
<tr><th>Samples</th><td><input name="screenshots" size="3" type="number" min="0"> <span class="small">count — for video: screenshots; manga: pages; music: audio clips. Blank = inherit default (6).</span></td></tr>
<tr><th>Sec/sample</th><td><input name="sample_seconds" size="3" type="number" min="1"> <span class="small">audio-only: duration of each clip. Blank = inherit default (5).</span></td></tr>
<tr><th>Banned extensions</th><td><textarea name="banned_extensions" rows="2" placeholder="(leave blank to inherit the default blocklist)"></textarea><div class="small">One per line. Blank = use the hardcoded default; non-empty list replaces the default outright.</div></td></tr>
<tr><th>PAR2 %</th><td><input name="par2_redundancy" size="3" type="number" min="0" max="100"> <span class="small">blank = inherit global default</span></td></tr>
<tr><th>Obfuscate</th><td><select name="obfuscate"><option value="">inherit</option><option value="1">yes</option><option value="0">no</option></select></td></tr>
<tr><th>Watermark</th><td><input name="watermark_text" placeholder="-YourTag"> <span class="small">drawn on every screenshot; blank = off</span></td></tr>
</table>
<p><button type="submit" class="primary">Create group</button></p>
</form>
{{end}}
`, groupsTmplFuncs)

var watchesTmpl = buildPageTemplate("watches", `
{{define "title"}}Watch Folders{{end}}
{{define "content"}}
<p class="lead">
Folders the offline pipeline scans for new files. Each folder is tagged
with a group, which decides where the resulting NZB gets posted. Paths
must be absolute.
</p>

<h2>Existing</h2>
{{if .Watches}}
<table>
<tr><th>Path</th><th>Group</th><th>Enabled</th><th></th></tr>
{{range .Watches}}
<tr>
<form class="row-form" method="post" action="/watches/update">
  <input type="hidden" name="id" value="{{.ID}}">
  <input type="hidden" name="enabled_present" value="1">
  <td><input name="path" value="{{.Path}}" size="40" required></td>
  <td>
    <select name="group_id">
      <option value=""{{if not .GroupID}} selected{{end}}>— unassigned —</option>
      {{$selected := .GroupID}}
      {{range $.Groups}}
        <option value="{{.ID}}"{{if isGroup $selected .ID}} selected{{end}}>{{.Name}}</option>
      {{end}}
    </select>
  </td>
  <td><input type="checkbox" name="enabled" value="1"{{if .Enabled}} checked{{end}}></td>
  <td>
    <button type="submit" class="primary">Save</button>
</form>
<form class="row-form" method="post" action="/watches/delete" style="display:inline;" onsubmit="return confirm('Delete watch {{.Path}}?')">
  <input type="hidden" name="id" value="{{.ID}}">
  <button type="submit" class="danger">Delete</button>
</form>
  </td>
</tr>
{{end}}
</table>
{{else}}
<p class="small">No watch folders configured yet.</p>
{{end}}

<h2 style="margin-top:28px;">Create new</h2>
{{if .Groups}}
<form method="post" action="/watches/create">
<input type="hidden" name="enabled_present" value="1">
<table>
<tr><th>Path</th><td><input name="path" placeholder="/data/watch/anime" size="40" required></td></tr>
<tr><th>Group</th><td>
  <select name="group_id" required>
    <option value="">— pick a group —</option>
    {{range .Groups}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
  </select>
</td></tr>
<tr><th>Enabled</th><td><input type="checkbox" name="enabled" value="1" checked></td></tr>
</table>
<p><button type="submit" class="primary">Add watch</button></p>
</form>
{{else}}
<p style="color:var(--warn);">Create at least one <a href="/groups" style="color:var(--blue);">group</a> first, then come back to wire a folder to it.</p>
{{end}}
{{end}}
`, watchesTmplFuncs)

var jobsTmpl = buildPageTemplate("jobs", `
{{define "title"}}Offline Jobs{{end}}
{{define "content"}}
<p class="lead">
Every file detected in a watch folder becomes a job. The processor runs
them end-to-end: stage → PAR2 → optional encrypt → upload → NZB written
to the output dir. Retry a failed job once the underlying issue is fixed;
deleting a row lets the watcher re-queue the same source file on next scan.
</p>

{{if .Jobs}}
<table>
<thead>
<tr><th>Title</th><th>Group</th><th>Status</th><th>Created</th><th>Error</th><th></th></tr>
</thead>
<tbody>
{{range .Jobs}}
<tr>
  <td>{{.Title}}<div class="small">{{.SourcePath}}</div></td>
  <td>{{.GroupNameAtCreation}}</td>
  <td><span class="badge {{statusClass .Status}}">{{.Status}}</span>{{if .Phase}} <span class="small">{{.Phase}}</span>{{end}}</td>
  <td class="small">{{.CreatedAt.Local.Format "Jan 02 15:04:05"}}</td>
  <td style="color:var(--red);" class="small">{{.Error}}</td>
  <td>
    {{if or (eq .Status "failed") (eq .Status "completed")}}
    <form class="row-form" method="post" action="/jobs/retry" style="display:inline;">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit" class="primary">Retry</button>
    </form>
    {{end}}
    <form class="row-form" method="post" action="/jobs/delete" style="display:inline;" onsubmit="return confirm('Delete job for {{.Title}}?')">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit" class="danger">Delete</button>
    </form>
  </td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="small">No jobs yet. Drop a file into a watch folder and give it a moment.</p>
{{end}}
{{end}}
`, jobsTmplFuncs)

var localUITmpl = buildPageTemplate("config", `
{{define "title"}}Config{{end}}
{{define "content"}}
<p class="lead">
Edit the layered agent config and per-tracker passkeys. Changes here are
persisted on this machine only; passkeys never leave the host.
{{if not .Writable}}<br><strong style="color:var(--warn);">config.yml is read-only</strong> — check file permission / Docker bind-mount.{{end}}
</p>

<h2>Config (config.yml)</h2>
<form id="cf">
<table>
<tr><th>Key</th><th>Value</th><th>Source</th></tr>
{{range .Rows}}
<tr>
  <td><code>{{.Key}}</code></td>
  <td><input name="{{.Key}}" value="{{.Value}}" size="30"></td>
  <td><span class="badge {{.Source}}">{{.Source}}</span></td>
</tr>
{{end}}
</table>
<p><button type="submit" class="primary" {{if not .Writable}}disabled{{end}}>Save to config.yml</button>
<span id="cs" class="small"></span></p>
</form>

<h2 style="margin-top:28px;">Site-side Overrides (web tier)</h2>
<p class="small">
Values the site has set for this agent. Edited here, written back to the site over the same
channel the agent polls with — no need to log into the site's admin dashboard to tweak an
override. Empty the value and save to clear an override.
{{if not .SiteConnected}}<br><strong style="color:var(--warn);">Site client not configured — form disabled.</strong>{{end}}
</p>
<table>
<tr><th>Key</th><th>Value</th><th></th></tr>
{{range $k, $v := .WebOverrides}}
<tr>
  <td><code>{{$k}}</code></td>
  <td>
    <form class="row-form" method="post" action="/config/web-override">
      <input type="hidden" name="key" value="{{$k}}">
      <input type="hidden" name="return_to" value="/">
      <input name="value" value="{{$v}}" size="30">
      <button type="submit" class="primary" {{if not $.SiteConnected}}disabled{{end}}>Save</button>
    </form>
  </td>
  <td>
    <form class="row-form" method="post" action="/config/web-override" style="display:inline;">
      <input type="hidden" name="key" value="{{$k}}">
      <input type="hidden" name="value" value="">
      <input type="hidden" name="return_to" value="/">
      <button type="submit" class="danger" {{if not $.SiteConnected}}disabled{{end}}>Clear</button>
    </form>
  </td>
</tr>
{{else}}
<tr><td colspan="3" class="small">No site-side overrides set.</td></tr>
{{end}}
</table>
<form method="post" action="/config/web-override" style="margin-top:12px;">
  <input type="hidden" name="return_to" value="/">
  <input name="key" placeholder="max_concurrent_downloads" required size="30">
  <input name="value" placeholder="new value" size="20">
  <button type="submit" class="primary" {{if not .SiteConnected}}disabled{{end}}>Add override</button>
</form>

<h2 style="margin-top:28px;">Private Trackers (secrets.yml, 0600)</h2>
<p class="small">Passkeys stay on this machine only. The site sees <em>that</em> trackers are configured, not the keys.</p>
<table id="sec">
<tr><th>Host</th><th></th></tr>
{{range .SecretsHosts}}
<tr><td>{{.}}</td><td><button type="button" onclick="delHost('{{.}}')" class="danger">remove</button></td></tr>
{{else}}
<tr><td colspan="2" class="small">No trackers configured.</td></tr>
{{end}}
</table>
<form id="sf" style="margin-top:12px;">
<input name="host" placeholder="nekobt.to" required>
<input name="key" placeholder="your passkey" required size="40">
<button type="submit" class="primary">Add / update</button>
<span id="ss" class="small"></span>
</form>

<script>
const cs = document.getElementById('cs');
document.getElementById('cf').addEventListener('submit', async e => {
    e.preventDefault();
    const body = new FormData(e.target);
    const r = await fetch('/config', { method: 'POST', body: new URLSearchParams(body) });
    cs.textContent = r.ok ? ' saved.' : ' error.';
    setTimeout(() => cs.textContent = '', 2000);
});
const ss = document.getElementById('ss');
document.getElementById('sf').addEventListener('submit', async e => {
    e.preventDefault();
    const body = new FormData(e.target);
    const r = await fetch('/secrets', { method: 'POST', body: new URLSearchParams(body) });
    if (r.ok) location.reload();
    else ss.textContent = ' error.';
});
async function delHost(h) {
    const body = new URLSearchParams({ host: h, key: '' });
    const r = await fetch('/secrets', { method: 'POST', body });
    if (r.ok) location.reload();
}
</script>
{{end}}
`, nil)

func (u *LocalUI) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	type row struct{ Key, Value, Source string }
	rows := make([]row, 0, len(config.ConfigYmlKeys))
	for _, k := range config.ConfigYmlKeys {
		rows = append(rows, row{Key: k, Value: u.cfg.Layered.Effective(k), Source: sourceFor(u.cfg.Layered, k)})
	}
	data := u.baseData("config", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Writable"] = u.cfg.Layered.Writable()
	data["Rows"] = rows
	data["SecretsHosts"] = u.secrets.List()
	data["WebOverrides"] = u.cfg.Layered.WebSnapshot()
	data["SiteConnected"] = u.site != nil
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = localUITmpl.Execute(w, data)
}

// ── Groups ────────────────────────────────────────────────────────────────
//
// Groups are edited exclusively from the local UI for now (no /api prefix,
// no auth) because the whole localui is loopback-only. Once we add an auth
// layer (planned for when the offline pipeline goes GA) the same handlers
// can mount under an authenticated mux without being rewritten.

func (u *LocalUI) handleGroups(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	groups, err := u.db.ListGroups()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := u.baseData("groups", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Groups"] = groups
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = groupsTmpl.Execute(w, data)
}

func (u *LocalUI) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	g, err := groupFromForm(r)
	if err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	if err := u.db.CreateGroup(g); err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	redirectGroups(w, r, "created "+g.Name, "")
}

func (u *LocalUI) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	g, err := groupFromForm(r)
	if err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	if err := u.db.UpdateGroup(g); err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	redirectGroups(w, r, "updated "+g.Name, "")
}

func (u *LocalUI) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectGroups(w, r, "", "invalid id")
		return
	}
	if err := u.db.DeleteGroup(id); err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	redirectGroups(w, r, "deleted", "")
}

// groupFromForm parses the shared create/update form into a Group. The id
// field is optional (create path leaves it 0); everything else is validated
// in storage.validateGroup so this stays thin.
func groupFromForm(r *http.Request) (*storage.Group, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	g := &storage.Group{Source: "local"}
	if s := r.PostFormValue("id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid id: %v", err)
		}
		g.ID = id
	}
	g.Name = r.PostFormValue("name")
	// Newsgroups entered one per line in the textarea, but accept commas
	// and whitespace too so copy-paste from anywhere works.
	raw := r.PostFormValue("newsgroups")
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	}) {
		g.Newsgroups = append(g.Newsgroups, line)
	}
	g.Screenshots = parseOptInt(r.PostFormValue("screenshots"))
	g.Par2Redundancy = parseOptInt(r.PostFormValue("par2_redundancy"))
	if v := r.PostFormValue("obfuscate"); v != "" {
		b := v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		g.Obfuscate = &b
	}
	g.WatermarkText = strings.TrimSpace(r.PostFormValue("watermark_text"))
	g.Type = strings.TrimSpace(r.PostFormValue("type"))
	g.SampleSeconds = parseOptInt(r.PostFormValue("sample_seconds"))
	// Banned extensions: accept one-per-line or comma-separated, validate
	// normalises dots and case so the operator can paste loose input.
	rawBans := r.PostFormValue("banned_extensions")
	for _, ext := range strings.FieldsFunc(rawBans, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	}) {
		g.BannedExtensions = append(g.BannedExtensions, ext)
	}
	return g, nil
}

// parseOptInt returns nil for blank input so "inherit global default"
// survives a form round-trip; non-empty but invalid falls through to nil
// for the same reason — the UI's numeric input prevents that in practice.
func parseOptInt(s string) *int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &i
}

func redirectGroups(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	redirectWithFlash(w, r, "/groups", msg, errMsg)
}

// ── Watch folders ─────────────────────────────────────────────────────────
//
// Same UI pattern as /groups: one row per watch with an inline update form,
// a create form below, and flash messages via redirect query params. The
// polling watcher goroutine (added in a later commit) consumes the same
// rows via ListActiveWatches.

func (u *LocalUI) handleWatches(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	watches, err := u.db.ListWatches()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Groups are fetched so the edit/create forms can render a <select>
	// with human names rather than raw ids; N+1 avoided by doing it once.
	groups, err := u.db.ListGroups()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := u.baseData("watches", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Watches"] = watches
	data["Groups"] = groups
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = watchesTmpl.Execute(w, data)
}

func (u *LocalUI) handleWatchCreate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	watch, err := watchFromForm(r)
	if err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	if err := u.db.CreateWatch(watch); err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	redirectWatches(w, r, "added "+watch.Path, "")
}

func (u *LocalUI) handleWatchUpdate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	watch, err := watchFromForm(r)
	if err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	if err := u.db.UpdateWatch(watch); err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	redirectWatches(w, r, "updated "+watch.Path, "")
}

func (u *LocalUI) handleWatchDelete(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectWatches(w, r, "", "invalid id")
		return
	}
	if err := u.db.DeleteWatch(id); err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	redirectWatches(w, r, "deleted", "")
}

func watchFromForm(r *http.Request) (*storage.WatchFolder, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	wf := &storage.WatchFolder{}
	if s := r.PostFormValue("id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid id: %v", err)
		}
		wf.ID = id
	}
	wf.Path = r.PostFormValue("path")
	if s := strings.TrimSpace(r.PostFormValue("group_id")); s != "" {
		gid, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid group id: %v", err)
		}
		wf.GroupID = &gid
	}
	// HTML checkboxes don't post when unchecked — the hidden "enabled_present"
	// field lets us distinguish "form submitted with box unchecked" from
	// "field missing entirely" so the update handler can flip enabled to 0.
	if r.PostFormValue("enabled_present") != "" {
		wf.Enabled = r.PostFormValue("enabled") == "1"
	} else {
		wf.Enabled = true
	}
	return wf, nil
}

func redirectWatches(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	redirectWithFlash(w, r, "/watches", msg, errMsg)
}

// ── Offline jobs ──────────────────────────────────────────────────────────
//
// Read-only list for now plus retry/delete. The processor lands in the
// next commit; until then rows only ever transition between 'queued' and
// 'failed' via retry, never to 'completed'.

func (u *LocalUI) handleJobs(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	jobs, err := u.db.ListOfflineJobs(100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := u.baseData("jobs", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Jobs"] = jobs
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = jobsTmpl.Execute(w, data)
}

func (u *LocalUI) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectJobs(w, r, "", "invalid id")
		return
	}
	if err := u.db.ResetQueuedJob(id); err != nil {
		redirectJobs(w, r, "", err.Error())
		return
	}
	redirectJobs(w, r, "requeued", "")
}

func (u *LocalUI) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectJobs(w, r, "", "invalid id")
		return
	}
	if err := u.db.DeleteJob(id); err != nil {
		redirectJobs(w, r, "", err.Error())
		return
	}
	redirectJobs(w, r, "deleted", "")
}

func redirectJobs(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	redirectWithFlash(w, r, "/jobs", msg, errMsg)
}

// ── Live events (SSE) ─────────────────────────────────────────────────────
//
// /events streams the agent's current speed/state/job-count snapshot every
// ~1.5s. Every page subscribes via EventSource in the layout's <script>
// block and updates the sidebar in place — no polling, no page reloads.
// The 5s site-post loop is the authoritative aggregation cadence; SSE
// pushes whatever snapshot is most recent, so clients see up-to-5s-old
// numbers in between aggregations. Good enough for a sidebar; the graph
// just repeats a point until the next aggregation.
func (u *LocalUI) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx/gluetun proxies from buffering

	rc := http.NewResponseController(w)
	// The server has a 10s WriteTimeout that would kill a long-lived SSE
	// connection. Disable it on this response only.
	_ = rc.SetWriteDeadline(time.Time{})

	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	writeEvent := func() bool {
		snap := GetLiveSnapshot()
		payload := map[string]any{
			"phase":            snap.Phase,
			"task_title":       snap.TaskTitle,
			"download_mbps":    snap.DownloadMBps,
			"upload_mbps":      snap.UploadMBps,
			"vpn_status":       snap.VPNStatus,
			"public_ip":        snap.PublicIP,
			"disk_free_gb":     snap.DiskFreeGB,
			"disk_reserved_gb": snap.DiskReservedGB,
			"disk_total_gb":    snap.DiskTotalGB,
		}
		if u.db != nil {
			// Job counts per status — drive sidebar badges and also catches
			// the case where the operator opens the UI mid-job to see "1
			// processing" without having to refresh.
			if counts, err := u.db.CountJobsByStatus(); err == nil {
				payload["jobs"] = counts
			}
		}
		body, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", body); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	// Send an initial event immediately so the sidebar doesn't show zeros
	// for up to 1.5s after page load.
	if !writeEvent() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !writeEvent() {
				return
			}
		}
	}
}

// redirectWithFlash is the shared tail for all CRUD handlers — POST-Redirect-GET
// with a single query param carrying the success or error message. Keeps
// flash state out of sessions/cookies, which the local UI doesn't have.
func redirectWithFlash(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	q := ""
	if msg != "" {
		q = "?msg=" + template.URLQueryEscaper(msg)
	} else if errMsg != "" {
		q = "?err=" + template.URLQueryEscaper(errMsg)
	}
	http.Redirect(w, r, path+q, http.StatusSeeOther)
}
