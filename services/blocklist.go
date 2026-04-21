package services

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBlockedExtensions is the fallback blocklist applied when a group
// doesn't specify its own. Lifted verbatim out of main.go's hardcoded map
// so both the online (site-polling) and offline (watch-folder) paths can
// share it, and so per-group overrides can replace it entirely when the
// group's content makes the default wrong (e.g. a music group that
// legitimately ships .iso alongside audio).
//
// The list is the Microsoft "high-risk file types" set plus a handful of
// common scripting / executable formats. Extensions are stored with the
// leading dot and in lowercase; RemoveBlockedFiles lowercases the runtime
// extension before the lookup.
var DefaultBlockedExtensions = map[string]bool{
	".ade": true, ".adp": true, ".app": true, ".application": true, ".appref-ms": true,
	".asp": true, ".aspx": true, ".asx": true, ".bas": true, ".bat": true, ".bgi": true,
	".cab": true, ".cer": true, ".chm": true, ".cmd": true, ".cnt": true, ".com": true,
	".cpl": true, ".crt": true, ".csh": true, ".der": true, ".diagcab": true, ".exe": true,
	".fxp": true, ".gadget": true, ".grp": true, ".hlp": true, ".hpj": true, ".hta": true,
	".htc": true, ".inf": true, ".ins": true, ".iso": true, ".isp": true, ".its": true,
	".jar": true, ".jnlp": true, ".js": true, ".jse": true, ".ksh": true, ".lnk": true,
	".mad": true, ".maf": true, ".mag": true, ".mam": true, ".maq": true, ".mar": true,
	".mas": true, ".mat": true, ".mau": true, ".mav": true, ".maw": true, ".mcf": true,
	".mda": true, ".mdb": true, ".mde": true, ".mdt": true, ".mdw": true, ".mdz": true,
	".msc": true, ".msh": true, ".msh1": true, ".msh2": true, ".mshxml": true,
	".msh1xml": true, ".msh2xml": true, ".msi": true, ".msp": true, ".mst": true,
	".msu": true, ".ops": true, ".osd": true, ".pcd": true, ".pif": true, ".pl": true,
	".plg": true, ".prf": true, ".prg": true, ".printerexport": true, ".ps1": true,
	".ps1xml": true, ".ps2": true, ".ps2xml": true, ".psc1": true, ".psc2": true,
	".psd1": true, ".psdm1": true, ".pst": true, ".py": true, ".pyc": true, ".pyo": true,
	".pyw": true, ".pyz": true, ".pyzw": true, ".reg": true, ".scf": true, ".scr": true,
	".sct": true, ".shb": true, ".shs": true, ".sln": true, ".theme": true, ".tmp": true,
	".url": true, ".vb": true, ".vbe": true, ".vbp": true, ".vbs": true, ".vcxproj": true,
	".vhd": true, ".vhdx": true, ".vsmacros": true, ".vsw": true, ".webpnp": true,
	".website": true, ".ws": true, ".wsc": true, ".wsf": true, ".wsh": true, ".xbap": true,
	".xll": true, ".xnk": true,
}

// EffectiveBlocklist chooses the blocklist a pipeline invocation should
// enforce: if the group provided an explicit list, that replaces the
// default outright; otherwise we fall back to DefaultBlockedExtensions.
// Passing `nil` or an empty slice for groupList means "use default," so
// callers outside the offline path (the online site-polling pipeline,
// one-off tools) get safe behaviour by default.
//
// The returned map is fresh on every call when a group list was given —
// mutating it in the caller won't leak into the default or other groups.
func EffectiveBlocklist(groupList []string) map[string]bool {
	if len(groupList) == 0 {
		return DefaultBlockedExtensions
	}
	m := make(map[string]bool, len(groupList))
	for _, ext := range groupList {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		m[ext] = true
	}
	return m
}

// RemoveBlockedFiles walks dir and deletes any file whose extension is
// in blocklist. Returns the count of files removed. Errors while walking
// are logged but don't abort the pass — the callers treat blocklist
// enforcement as best-effort; the worst case is that a single risky file
// slips through to the uploader, which is still a staging step.
func RemoveBlockedFiles(dir string, blocklist map[string]bool) int {
	if blocklist == nil {
		blocklist = DefaultBlockedExtensions
	}
	removed := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if blocklist[ext] {
			log.Printf("Removing blocked file: %s", info.Name())
			_ = os.Remove(path)
			removed++
		}
		return nil
	})
	return removed
}
