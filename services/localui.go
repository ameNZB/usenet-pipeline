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

	"github.com/ameNZB/usenet-pipeline/config"
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
	port     int
	bindAddr string
}

func StartLocalUI(cfg *config.Config, secrets *SecretsStore) *LocalUI {
	port, _ := strconv.Atoi(os.Getenv("LOCAL_UI_PORT"))
	if port <= 0 {
		return nil
	}
	bind := os.Getenv("LOCAL_UI_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	ui := &LocalUI{cfg: cfg, secrets: secrets, port: port, bindAddr: bind}
	addr := net.JoinHostPort(bind, strconv.Itoa(port))

	mux := http.NewServeMux()
	mux.HandleFunc("/", ui.handleIndex)
	mux.HandleFunc("/config", ui.handleConfig)
	mux.HandleFunc("/secrets", ui.handleSecrets)
	mux.HandleFunc("/status", ui.handleStatus)

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

var localUITmpl = template.Must(template.New("localui").Parse(`<!doctype html>
<html><head>
<meta charset="utf-8"><title>Agent Local UI</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 900px; margin: 2em auto; padding: 0 1em; background: #111; color: #eee; }
h1, h2 { font-weight: 500; }
table { width: 100%; border-collapse: collapse; font-size: 0.9em; }
th, td { padding: 6px 8px; border-bottom: 1px solid #333; text-align: left; }
input, button { background: #222; color: #eee; border: 1px solid #444; padding: 4px 8px; border-radius: 3px; }
button { cursor: pointer; }
button.primary { background: #336; border-color: #557; }
.badge { display: inline-block; padding: 1px 6px; font-size: 0.7em; border-radius: 3px; background: #333; }
.badge.env { background: #553; } .badge.yml { background: #255; } .badge.default { background: #333; }
.warn { color: #fb3; font-size: 0.8em; }
</style>
</head><body>
<h1>Agent Local UI</h1>
<p class="warn">Running on {{.BindAddr}}. Anything edited here is persisted on this machine and never leaves it.
{{if not .Writable}}<br><strong>config.yml is read-only</strong> — check the file permission / Docker bind-mount.{{end}}</p>

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
<span id="cs"></span></p>
</form>

<h2>Private Trackers (secrets.yml, 0600)</h2>
<p style="font-size:0.8em;color:#999;">
Passkeys stay on this machine only. The site sees <em>that</em> trackers are configured, not the keys.
</p>
<table id="sec">
<tr><th>Host</th><th></th></tr>
{{range .SecretsHosts}}
<tr><td>{{.}}</td><td><button type="button" onclick="delHost('{{.}}')">remove</button></td></tr>
{{else}}
<tr><td colspan="2" style="color:#999;">No trackers configured.</td></tr>
{{end}}
</table>
<form id="sf" style="margin-top:1em;">
<input name="host" placeholder="nekobt.to" required>
<input name="key" placeholder="your passkey" required size="40">
<button type="submit" class="primary">Add / update</button>
<span id="ss"></span>
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
</body></html>`))

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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = localUITmpl.Execute(w, map[string]any{
		"BindAddr":     u.bindAddr,
		"Writable":     u.cfg.Layered.Writable(),
		"Rows":         rows,
		"SecretsHosts": u.secrets.List(),
	})
}
