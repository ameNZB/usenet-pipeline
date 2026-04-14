package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ameNZB/usenet-pipeline/client"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/services"
	"github.com/ameNZB/usenet-pipeline/storage"
)

// ── Live status: aggregated across all concurrent tasks ─────────────────────

var (
	liveStatusMu sync.RWMutex
	liveStatus   = client.AgentLiveStatus{Phase: "idle"}

	// Per-task progress tracking for the dashboard.
	taskProgressMu sync.RWMutex
	taskProgress   = map[int64]*client.FileProgress{} // keyed by request ID
)

func setLivePhase(phase string) {
	liveStatusMu.Lock()
	liveStatus.Phase = phase
	liveStatusMu.Unlock()
}

func setLiveTask(title string, requestID int64) {
	liveStatusMu.Lock()
	liveStatus.TaskTitle = title
	liveStatus.RequestID = requestID
	liveStatusMu.Unlock()
}

func clearLiveStatus() {
	liveStatusMu.Lock()
	liveStatus = client.AgentLiveStatus{Phase: "idle"}
	liveStatusMu.Unlock()
}

// updateTaskProgress sets the per-task progress entry for aggregation.
func updateTaskProgress(requestID int64, fp *client.FileProgress) {
	taskProgressMu.Lock()
	if fp == nil {
		delete(taskProgress, requestID)
	} else {
		taskProgress[requestID] = fp
	}
	taskProgressMu.Unlock()
}

// aggregateLiveStatus rebuilds the live status from all active task progress entries.
func aggregateLiveStatus() {
	taskProgressMu.RLock()
	files := make([]client.FileProgress, 0, len(taskProgress))
	var dlSpeed, ulSpeed float64
	downloading, uploading := 0, 0
	for _, fp := range taskProgress {
		files = append(files, *fp)
		// Parse speed from the formatted string.
		var spd float64
		fmt.Sscanf(fp.Speed, "%f", &spd)
		if fp.Phase == "downloading" {
			dlSpeed += spd
			downloading++
		} else if fp.Phase == "uploading" {
			ulSpeed += spd
			uploading++
		}
	}
	count := len(taskProgress)
	taskProgressMu.RUnlock()

	liveStatusMu.Lock()
	liveStatus.Files = files
	if count == 0 {
		liveStatus.Phase = "idle"
		liveStatus.DownloadSpeed = ""
		liveStatus.UploadSpeed = ""
	} else {
		if uploading > 0 {
			liveStatus.Phase = "uploading"
		} else if downloading > 0 {
			liveStatus.Phase = "downloading"
		} else {
			liveStatus.Phase = "processing"
		}
		if dlSpeed > 0 {
			liveStatus.DownloadSpeed = fmt.Sprintf("%.2f MB/s", dlSpeed)
		} else {
			liveStatus.DownloadSpeed = ""
		}
		if ulSpeed > 0 {
			liveStatus.UploadSpeed = fmt.Sprintf("%.2f MB/s", ulSpeed)
		} else {
			liveStatus.UploadSpeed = ""
		}
	}
	liveStatusMu.Unlock()
}

// startStatusReporter posts the agent's live status to the site every 5 seconds.
func startStatusReporter(site *client.SiteClient, tempDir string) {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		aggregateLiveStatus()

		liveStatusMu.RLock()
		storage.GlobalState.RLock()
		snap := liveStatus
		snap.VPNStatus = storage.GlobalState.VPNStatus
		snap.PublicIP = storage.GlobalState.PublicIP
		storage.GlobalState.RUnlock()
		liveStatusMu.RUnlock()

		// Disk usage for dashboard display.
		if free, err := services.FreeDiskSpace(tempDir); err == nil {
			snap.DiskFreeGB = float64(free) / 1024 / 1024 / 1024
		}
		snap.DiskReservedGB = float64(services.TotalReservedBytes()) / 1024 / 1024 / 1024

		resp, _ := site.PostStatus(snap)
		if resp != nil && resp.CancelRequestID > 0 {
			jobName := fmt.Sprintf("request-%d", resp.CancelRequestID)
			if cancelFn, ok := storage.JobCancels.Load(jobName); ok {
				log.Printf("[skip] Cancelling task %s (request %d) by user request", jobName, resp.CancelRequestID)
				cancelFn.(context.CancelFunc)()
			}
		}
	}
}

// startSpeedLogger periodically logs download/upload speeds for all active tasks.
func startSpeedLogger() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		taskProgressMu.RLock()
		if len(taskProgress) == 0 {
			taskProgressMu.RUnlock()
			continue
		}
		for rid, fp := range taskProgress {
			log.Printf("[speed] request=%d phase=%s speed=%s percent=%.1f%% title=%s",
				rid, fp.Phase, fp.Speed, fp.Percent, fp.Name)
		}
		taskProgressMu.RUnlock()
	}
}

// ── Upload serialization ────────────────────────────────────────────────────

// uploadMu ensures only one task uploads to Usenet at a time (shared NNTP connections).
var uploadMu sync.Mutex

// ── Active task tracking (prevents double-dispatch) ─────────────────────────

var (
	activeTasks   = map[int64]bool{}
	activeTasksMu sync.Mutex
)

func claimTask(id int64) bool {
	activeTasksMu.Lock()
	defer activeTasksMu.Unlock()
	if activeTasks[id] {
		return false
	}
	activeTasks[id] = true
	return true
}

func releaseTask(id int64) {
	activeTasksMu.Lock()
	delete(activeTasks, id)
	activeTasksMu.Unlock()
}

func activeTaskCount() int {
	activeTasksMu.Lock()
	defer activeTasksMu.Unlock()
	return len(activeTasks)
}

// applyDirective runs a queued site directive (currently only write_config,
// which edits config.yml on disk) and acks the outcome. Errors are reported
// via ack so the site can surface them in the settings UI; the agent keeps
// polling regardless.
func applyDirective(cfg *config.Config, site *client.SiteClient, d client.Directive) {
	switch d.Kind {
	case "write_config":
		var p client.WriteConfigPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			site.AckDirective(d.ID, "bad payload: "+err.Error())
			return
		}
		if cfg.Layered == nil {
			site.AckDirective(d.ID, "layered config not initialised")
			return
		}
		written, err := cfg.Layered.WriteYml(p.Updates)
		if err != nil {
			site.AckDirective(d.ID, err.Error())
			return
		}
		// Re-derive effective values so env/yml changes take effect now.
		cfg.Refresh()
		log.Printf("write_config directive %d applied: %v", d.ID, written)
		site.AckDirective(d.ID, "")
	default:
		site.AckDirective(d.ID, "unknown directive kind: "+d.Kind)
	}
}

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	cfg := config.NewConfig()

	if cfg.SiteURL == "" || cfg.AgentToken == "" {
		log.Fatal("SITE_URL and AGENT_TOKEN must be set")
	}

	for _, dir := range []string{cfg.TempDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}
	services.InitDiskLimit(cfg.MaxDiskUsageGB)

	storage.StateFile = filepath.Join(cfg.TempDir, "state.json")
	storage.LoadState()

	go services.MonitorNetworkConnection(cfg)

	// Secrets + optional local UI. Both are no-ops when the operator hasn't
	// enabled them (LOCAL_UI_PORT unset, secrets.yml missing).
	secrets := services.LoadSecrets()
	localUI := services.StartLocalUI(cfg, secrets)

	site := client.New(cfg)
	pollInterval := time.Duration(cfg.PollInterval) * time.Second

	// On startup, clear any stale locks from a previous crash.
	if cleared, err := site.ClearMyLocks(); err != nil {
		log.Printf("Warning: could not clear stale locks: %v", err)
	} else if cleared > 0 {
		log.Printf("Cleared %d stale lock(s) from previous run", cleared)
	}

	go startStatusReporter(site, cfg.TempDir)
	go startSpeedLogger()

	maxDL := cfg.MaxConcurrentDownloads
	if maxDL < 1 {
		maxDL = 1
	}
	log.Printf("Agent started — polling %s every %ds (max %d concurrent downloads)",
		cfg.SiteURL, cfg.PollInterval, maxDL)

	const minFreeSpaceGB = 1 // don't accept new tasks below this threshold
	var lastReason string    // dedup repeated poll reasons in logs

	for {
		// Fetch remote config from server (falls back to local env vars).
		// We do this BEFORE the capacity check so a max_concurrent change
		// from the dashboard takes effect within one poll interval instead
		// of waiting for the next restart.
		cpuThreshold := cfg.CPUMaxPercent
		var remoteCfg *client.RemoteConfig
		if rc, err := site.GetConfig(); err == nil {
			remoteCfg = rc
			if rc.CpuMaxPercent > 0 {
				cpuThreshold = float64(rc.CpuMaxPercent)
			}
			// Apply remote max-concurrent override. 0 means "use local default".
			if rc.MaxConcurrent > 0 && rc.MaxConcurrent != maxDL {
				log.Printf("max concurrent downloads updated by site: %d -> %d", maxDL, rc.MaxConcurrent)
				maxDL = rc.MaxConcurrent
			}
			// Apply remote disk limit override.
			if rc.MaxDiskUsageGB > 0 {
				services.InitDiskLimit(rc.MaxDiskUsageGB)
			}
			// Layered web-override tier: only applied when the site provided
			// an explicit WebOverrides map. Empty/nil leaves existing tier in
			// place rather than clobbering it on every poll.
			if rc.WebOverrides != nil && cfg.Layered != nil {
				cfg.Layered.ApplyWeb(rc.WebOverrides)
				cfg.Refresh()
			}
		}

		// Best-effort: post our local config snapshot so the settings UI can
		// render state badges, and drain any pending write_config directives
		// the user queued from the site. Failures are non-fatal — the agent
		// keeps polling for work regardless.
		if cfg.Layered != nil {
			extras := []config.ReportExtra{
				config.WithPrivateTrackers(secrets.Has()),
				config.WithLocalUIURL(localUI.URL()),
			}
			if err := site.PostLocalConfig(cfg.Layered.Report(extras...)); err != nil {
				// Only log once per reason to avoid spamming on older sites
				// that don't yet implement /api/agent/local-config.
				if lastReason != "local_config_post" {
					log.Printf("local-config post: %v (ok if site not yet upgraded)", err)
					lastReason = "local_config_post"
				}
			}
			if dirs, err := site.FetchDirectives(); err == nil {
				for _, d := range dirs {
					applyDirective(cfg, site, d)
				}
			}
		}

		// Only poll for new work if we have capacity.
		if activeTaskCount() >= maxDL {
			time.Sleep(pollInterval)
			continue
		}

		// Skip polling if CPU usage is too high.
		if cpuThreshold > 0 {
			if cpuPct, err := services.CPUUsagePercent(); err == nil && cpuPct > cpuThreshold {
				if lastReason != "cpu_high" {
					log.Printf("CPU usage %.0f%% > %.0f%% threshold — pausing new tasks", cpuPct, cpuThreshold)
					site.PostLog("info", fmt.Sprintf("CPU usage %.0f%% exceeds %.0f%% threshold — pausing new tasks", cpuPct, cpuThreshold))
					lastReason = "cpu_high"
				}
				time.Sleep(pollInterval)
				continue
			}
		}

		// Skip polling if disk space (minus reservations for in-flight tasks) is too low.
		if effective, err := services.FreeDiskAfterReservations(cfg.TempDir); err == nil {
			effectiveGB := float64(effective) / 1024 / 1024 / 1024
			if effectiveGB < minFreeSpaceGB {
				if lastReason != "disk_low" {
					reserved := float64(services.TotalReservedBytes()) / 1024 / 1024 / 1024
					log.Printf("Low disk space: %.1f GB effective free (%.1f GB reserved by active tasks), need %d GB — waiting",
						effectiveGB, reserved, minFreeSpaceGB)
					lastReason = "disk_low"
				}
				time.Sleep(pollInterval)
				continue
			}
		}

		result, err := site.Poll()
		if err != nil {
			// Maintenance: sleep the ETA + a small buffer, don't spam logs.
			if me, ok := client.IsMaintenanceError(err); ok {
				wait := time.Duration(me.Info.ETASeconds+15) * time.Second
				if wait < 30*time.Second {
					wait = 30 * time.Second
				}
				if wait > 10*time.Minute {
					wait = 10 * time.Minute
				}
				if lastReason != "maintenance" {
					log.Printf("Site in maintenance: %s — waiting %s", me.Info.Reason, wait.Round(time.Second))
					lastReason = "maintenance"
				}
				time.Sleep(wait)
				continue
			}
			log.Printf("Poll error: %v", err)
			site.PostLog("error", "Poll error: "+err.Error())
			time.Sleep(pollInterval)
			continue
		}
		if lastReason == "maintenance" {
			lastReason = ""
		}

		if result.Command == "stop" {
			log.Printf("Received stop command — idling")
			time.Sleep(pollInterval)
			continue
		}

		// "stop_after_current" is a graceful shutdown: the user clicked
		// "Finish & Stop" in the dashboard, so we drain any in-flight
		// downloads and then exit cleanly. Until activeTaskCount() drops
		// to zero, behave exactly like "stop" — accept no new work but
		// keep the process alive so existing goroutines can finish.
		if result.Command == "stop_after_current" {
			active := activeTaskCount()
			if active == 0 {
				log.Printf("stop_after_current: no active tasks — shutting down")
				site.PostLog("info", "Graceful shutdown: no active tasks remaining")
				return
			}
			log.Printf("stop_after_current: waiting for %d active task(s) to finish", active)
			time.Sleep(pollInterval)
			continue
		}

		if result.Task == nil {
			if result.Reason != "" {
				log.Printf("No task: %s", result.Reason)
				// Log to site only occasionally to avoid spam (use a simple throttle).
				if lastReason != result.Reason {
					site.PostLog("info", "Poll: "+result.Reason)
					lastReason = result.Reason
				}
			}
			time.Sleep(pollInterval)
			continue
		}
		lastReason = "" // reset when we get a task

		task := result.Task
		// Prevent double-dispatch if site returns same task.
		if !claimTask(task.RequestID) {
			time.Sleep(pollInterval)
			continue
		}

		log.Printf("Received task: request=%d title=%q hash=%s", task.RequestID, task.Title, task.InfoHash)
		site.PostLog("info", fmt.Sprintf("Picked up request #%d: %s", task.RequestID, task.Title))
		go func(t *client.AgentTask, rc *client.RemoteConfig) {
			defer releaseTask(t.RequestID)
			processTask(cfg, site, t, rc)
		}(task, remoteCfg)

		time.Sleep(2 * time.Second) // brief pause before polling for next
	}
}

// videoExtensions for finding the main video file(s) in a download.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m2ts": true,
	".ts": true, ".wmv": true, ".flv": true, ".webm": true, ".mov": true,
}

// blockedExts contains dangerous file types that must not be uploaded.
var blockedExts = map[string]bool{
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

// removeBlockedFiles deletes files with dangerous extensions from a directory.
// Returns the number of files removed.
func removeBlockedFiles(dir string) int {
	removed := 0
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if blockedExts[ext] {
			log.Printf("Removing blocked file: %s", info.Name())
			os.Remove(path)
			removed++
		}
		return nil
	})
	return removed
}

// findVideoFiles returns all video files in a directory, sorted largest first.
func findVideoFiles(dir string) []string {
	type vf struct {
		path string
		size int64
	}
	var files []vf
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if videoExts[strings.ToLower(filepath.Ext(info.Name()))] {
			files = append(files, vf{path, info.Size()})
		}
		return nil
	})
	// Sort largest first — main video is typically the biggest file.
	for i := 0; i < len(files); i++ {
		for j := i + 1; j < len(files); j++ {
			if files[j].size > files[i].size {
				files[i], files[j] = files[j], files[i]
			}
		}
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	return paths
}

// dirSize walks a directory and sums file sizes.
func dirSize(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func processTask(cfg *config.Config, site *client.SiteClient, task *client.AgentTask, remoteCfg *client.RemoteConfig) {
	jobName := fmt.Sprintf("request-%d", task.RequestID)
	ctx, cancel := context.WithCancel(context.Background())
	storage.JobCancels.Store(jobName, cancel)
	defer storage.JobCancels.Delete(jobName)
	defer cancel()

	// Clean up per-task progress and disk reservation on exit.
	defer updateTaskProgress(task.RequestID, nil)
	defer services.ReleaseDisk(jobName)

	reportProgress := func(phase, details string) {
		storage.UpdateState(jobName, phase, details, 0)
		_ = site.ReportProgress(task.LockID, phase+": "+details, "")
	}

	fail := func(phase, msg string, err error) {
		log.Printf("[%d] %s: %v", task.RequestID, phase, err)
		reportProgress("Failed", msg+": "+err.Error())
		_ = site.Complete(client.CompleteResult{
			LockID:    task.LockID,
			RequestID: task.RequestID,
			Status:    "failed",
		})
	}

	// Per-task progress callback for live dashboard.
	var lastLockUpdate time.Time
	progressCb := func(speedMBs float64, percent float64, phase string, peers int) {
		updateTaskProgress(task.RequestID, &client.FileProgress{
			Name:    task.Title,
			Percent: percent,
			Speed:   fmt.Sprintf("%.2f MB/s", speedMBs),
			Phase:   phase,
			Peers:   peers,
		})
		// Throttle DB lock updates to every 10 seconds so the admin Active
		// Tasks table stays current without hammering the site API.
		if time.Since(lastLockUpdate) >= 10*time.Second {
			lastLockUpdate = time.Now()
			label := "Downloading"
			if phase == "uploading" {
				label = "Uploading"
			}
			progress := fmt.Sprintf("%s: %.0f%% (%.1f MB/s)", label, percent, speedMBs)
			if peers > 0 {
				progress += fmt.Sprintf(" [%d peers]", peers)
			}
			_ = site.ReportProgress(task.LockID, progress, fmt.Sprintf("%.1f MB/s", speedMBs))
		}
	}

	rid := task.RequestID
	taskStart := time.Now()
	log.Printf("[%d] ── Pipeline start: %s ──", rid, task.Title)
	reportProgress("Initializing", "Preparing pipeline...")

	// ── 1. Download torrent (or resume from previous download) ────────────
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s", task.InfoHash)

	// Check for existing download from a previous interrupted run.
	var downloadedPath string
	storage.GlobalState.RLock()
	if prev, ok := storage.GlobalState.Jobs[jobName]; ok && prev.DownloadedPath != "" {
		if info, err := os.Stat(prev.DownloadedPath); err == nil && info.IsDir() {
			downloadedPath = prev.DownloadedPath
			log.Printf("[%d] Resuming from previous download: %s", task.RequestID, downloadedPath)
		}
	}
	storage.GlobalState.RUnlock()

	if downloadedPath == "" {
		log.Printf("[%d] Downloading: %s", task.RequestID, task.Title)
		reportProgress("Downloading", "Fetching torrent metadata...")

		// Set per-task callback for download progress.
		services.SetProgressCallbackForJob(jobName, progressCb)

		dlOpts := &services.DownloadOpts{
			SlowThresholdMBs:    cfg.SlowSpeedThresholdMBs,
			SlowTimeoutMins:     cfg.SlowSpeedTimeoutMins,
			LowPeersThreshold:   -1, // disabled by default
			LowPeersTimeoutMins: 0,
			IsBoosted:           task.BoostCount > 0,
		}
		// Override from remote config if available.
		if remoteCfg != nil {
			if remoteCfg.SlowSpeedTimeout > 0 {
				dlOpts.SlowThresholdMBs = remoteCfg.SlowSpeedThreshold
				dlOpts.SlowTimeoutMins = remoteCfg.SlowSpeedTimeout
			}
			if remoteCfg.LowPeersTimeout > 0 {
				dlOpts.LowPeersThreshold = remoteCfg.LowPeersThreshold
				dlOpts.LowPeersTimeoutMins = remoteCfg.LowPeersTimeout
			}
		}
		var err error
		downloadedPath, err = services.DownloadMagnet(ctx, magnet, cfg, jobName, dlOpts)

		services.ClearProgressCallbackForJob(jobName)

		if err != nil {
			if errors.Is(err, services.ErrSlowDownload) {
				log.Printf("[%d] Slow download rejected — skipping", task.RequestID)
				reportProgress("Failed", "Download too slow — skipping")
				_ = site.Complete(client.CompleteResult{
					LockID:    task.LockID,
					RequestID: task.RequestID,
					Status:    "failed",
				})
			} else if ctx.Err() != nil {
				log.Printf("[%d] Task skipped by user", task.RequestID)
				reportProgress("Skipped", "Task skipped by user")
				_ = site.Complete(client.CompleteResult{
					LockID:    task.LockID,
					RequestID: task.RequestID,
					Status:    "failed",
				})
			} else {
				fail("Download", "Download error", err)
			}
			return
		}
		storage.UpdateJobMeta(jobName, downloadedPath, "", "")
		log.Printf("[%d] Download complete: %s", task.RequestID, downloadedPath)
	}
	defer os.RemoveAll(downloadedPath)

	// Remove any dangerous file types before processing.
	if n := removeBlockedFiles(downloadedPath); n > 0 {
		log.Printf("[%d] Removed %d blocked file(s) from download", task.RequestID, n)
	}

	// ── 2. Extract video metadata ──────────────────────────────────────────
	log.Printf("[%d] Step 2: Analyzing video metadata...", rid)
	reportProgress("Analyzing", "Extracting video metadata...")
	updateTaskProgress(task.RequestID, &client.FileProgress{
		Name: task.Title, Phase: "processing", Percent: 0,
	})

	var videoInfo *services.VideoInfo
	var screenshots []string
	videoFiles := findVideoFiles(downloadedPath)

	if len(videoFiles) > 0 {
		mainVideo := videoFiles[0] // largest video file

		info, err := services.ProbeVideo(ctx, mainVideo)
		if err != nil {
			log.Printf("[%d] Probe warning (non-fatal): %v", rid, err)
		} else {
			videoInfo = info
			log.Printf("[%d] Video: %s %dx%d %s %s %s",
				rid, info.VideoCodec, info.Width, info.Height,
				info.ResolutionLabel(), info.HDR, info.DurationStr())
		}

		// ── 3. Generate screenshots ────────────────────────────────────────
		if videoInfo != nil && videoInfo.Duration > 10 {
			log.Printf("[%d] Step 3: Generating screenshots...", rid)
			reportProgress("Screenshots", "Capturing preview images...")
			updateTaskProgress(task.RequestID, &client.FileProgress{
				Name: task.Title, Phase: "screenshots",
			})
			screenDir := filepath.Join(cfg.TempDir, "screens-"+services.GenerateRandomPassword(8))
			defer os.RemoveAll(screenDir)

			shots, err := services.GenerateScreenshots(ctx, mainVideo, screenDir, videoInfo.Duration, 6)
			if err != nil {
				log.Printf("[%d] Screenshot warning (non-fatal): %v", rid, err)
			} else {
				screenshots = shots
				log.Printf("[%d] Generated %d screenshots", rid, len(shots))
			}
		}
	}

	// ── 4. Prepare upload directory with obfuscated filenames ───────────────
	stageDir := filepath.Join(cfg.TempDir, services.GenerateRandomPassword(12))
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		fail("Prepare", "Stage dir error", err)
		return
	}
	defer os.RemoveAll(stageDir)

	log.Printf("[%d] Step 4: Staging files...", rid)
	if cfg.Obfuscate {
		reportProgress("Preparing", "Obfuscating filenames...")
		if err := services.ObfuscateFiles(ctx, downloadedPath, stageDir); err != nil {
			fail("Prepare", "Prepare error", err)
			return
		}
	} else {
		reportProgress("Preparing", "Copying files...")
		if err := services.CopyFiles(ctx, downloadedPath, stageDir); err != nil {
			fail("Prepare", "Prepare error", err)
			return
		}
	}
	log.Printf("[%d] Step 4: Staging complete", rid)

	// ── 5. Generate PAR2 recovery files ────────────────────────────────────
	log.Printf("[%d] Step 5: Generating PAR2 recovery data...", rid)
	reportProgress("PAR2", "Generating recovery data...")
	updateTaskProgress(task.RequestID, &client.FileProgress{
		Name: task.Title, Phase: "par2",
	})
	baseName := services.GenerateRandomPassword(12)
	if !cfg.Obfuscate {
		baseName = services.SanitizeBaseName(task.Title)
	}
	par2Start := time.Now()

	// Stream PAR2 progress to the dashboard so users can see it's not stuck.
	// par2create emits lines like "Processing: 12.3%" and "Creating recovery
	// file(s): 45.6%" that we parse and forward via the live status channel.
	par2Progress := func(phase string, pct float64) {
		elapsed := time.Since(par2Start).Round(time.Second)
		detail := fmt.Sprintf("%s %.0f%% (%s elapsed)", phase, pct, elapsed)
		reportProgress("PAR2", detail)
		updateTaskProgress(task.RequestID, &client.FileProgress{
			Name:    task.Title,
			Phase:   "par2",
			Percent: pct,
		})
	}

	_, err := services.GeneratePAR2(ctx, stageDir, baseName, services.PAR2Options{
		Redundancy: cfg.PAR2Redundancy,
		BlockSize:  services.ChunkSize,
		Threads:    cfg.PAR2Threads,
		MemoryMB:   cfg.PAR2Memory,
	}, par2Progress)
	if err != nil {
		log.Printf("[%d] PAR2 warning (non-fatal): %v", rid, err)
		reportProgress("PAR2", "PAR2 failed, uploading without recovery")
	} else {
		log.Printf("[%d] Step 5: PAR2 complete (%s)", rid, time.Since(par2Start).Round(time.Second))
	}

	// ── 6. Optional encryption ─────────────────────────────────────────────
	var password string
	uploadDir := stageDir
	if cfg.Encrypt {
		password = services.GenerateRandomPassword(16)
		archiveName := services.GenerateRandomPassword(16) + ".7z"
		archivePath := filepath.Join(cfg.TempDir, archiveName)
		defer os.Remove(archivePath)

		log.Printf("[%d] Step 6: Encrypting with 7z...", rid)
		reportProgress("Encrypting", "Creating password-protected 7z archive...")
		updateTaskProgress(task.RequestID, &client.FileProgress{
			Name: task.Title, Phase: "encrypting",
		})
		if err := services.EncryptWith7z(ctx, stageDir, archivePath, password); err != nil {
			fail("Encrypt", "Encryption error", err)
			return
		}

		encDir := filepath.Join(cfg.TempDir, "enc-"+services.GenerateRandomPassword(8))
		os.MkdirAll(encDir, 0755)
		defer os.RemoveAll(encDir)
		os.Rename(archivePath, filepath.Join(encDir, archiveName))
		uploadDir = encDir
		log.Printf("Encrypted to %s (%d chars password)", archiveName, len(password))
	}

	// ── 7. Upload all files to Usenet (serialized — one upload at a time) ──
	var totalUploadSize int64
	filepath.Walk(uploadDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			totalUploadSize += info.Size()
		}
		return nil
	})

	log.Printf("[%d] Step 7: Waiting for upload slot (%.1f MiB to upload)...", rid, float64(totalUploadSize)/1024/1024)
	reportProgress("Queued", "Waiting for upload slot...")
	updateTaskProgress(task.RequestID, &client.FileProgress{
		Name: task.Title, Phase: "queued", Size: totalUploadSize,
	})

	uploadMu.Lock()
	defer uploadMu.Unlock()

	log.Printf("[%d] Step 7: Uploading to Usenet: %.2f MiB via %d connections...",
		rid, float64(totalUploadSize)/1024/1024, cfg.NNTPConnections)
	reportProgress("Uploading", fmt.Sprintf("%.1f MiB via %d NNTP connections...",
		float64(totalUploadSize)/1024/1024, cfg.NNTPConnections))

	services.SetProgressCallbackForJob(jobName, progressCb)

	uploadStart := time.Now()
	fileSegments, err := services.UploadDirectory(ctx, cfg, uploadDir, jobName)

	services.ClearProgressCallbackForJob(jobName)

	if err != nil {
		fail("Upload", "Upload error", err)
		return
	}
	uploadDur := time.Since(uploadStart)
	speedMBs := float64(totalUploadSize) / 1024 / 1024 / uploadDur.Seconds()
	log.Printf("[%d] Step 7: Upload complete: %.2f MiB in %s (%.1f MB/s)",
		rid, float64(totalUploadSize)/1024/1024, uploadDur.Round(time.Second), speedMBs)

	// ── 8. Generate NZB ────────────────────────────────────────────────────
	log.Printf("[%d] Step 8: Generating NZB and reporting to site...", rid)
	reportProgress("Finalizing", "Generating NZB...")

	nzbData, err := services.CreateMultiFileNZBBytes(cfg, fileSegments, password, services.NZBMetaInfo{
		Title:     task.Title,
		RequestID: task.RequestID,
	})
	if err != nil {
		fail("NZB", "NZB error", err)
		return
	}

	// ── 9. Report completion with NZB + metadata + screenshots ─────────────
	reportProgress("Reporting", "Sending results to site...")

	completeResult := client.CompleteResult{
		LockID:      task.LockID,
		RequestID:   task.RequestID,
		Status:      "completed",
		NzbData:     nzbData,
		Password:    password,
		MediaInfo:   videoInfo,
		Screenshots: screenshots,
	}

	// Retry completion with smart handling for maintenance mode. Normal
	// errors retry up to 3 times with short backoff. If the site returns
	// a MaintenanceError (503 with {"maintenance":true,...}), we wait out
	// the reported ETA and keep retrying indefinitely — the agent has
	// already done the expensive upload work and we don't want to lose it.
	var completeErr error
	normalAttempt := 0
	for {
		completeErr = site.Complete(completeResult)
		if completeErr == nil {
			break
		}

		// Maintenance: wait out the ETA and keep going.
		if me, ok := client.IsMaintenanceError(completeErr); ok {
			wait := time.Duration(me.Info.ETASeconds+15) * time.Second
			if wait < 30*time.Second {
				wait = 30 * time.Second
			}
			if wait > 10*time.Minute {
				wait = 10 * time.Minute
			}
			log.Printf("[%d] Site in maintenance: %s — waiting %s before retry",
				rid, me.Info.Reason, wait.Round(time.Second))
			reportProgress("Waiting", fmt.Sprintf("Site maintenance: %s", me.Info.Reason))
			if ctx.Err() != nil {
				break
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				completeErr = ctx.Err()
			}
			continue
		}

		// Normal transient error: 3 quick retries then give up.
		normalAttempt++
		log.Printf("[%d] Completion attempt %d/3 failed: %v", rid, normalAttempt, completeErr)
		if normalAttempt >= 3 {
			break
		}
		time.Sleep(time.Duration(normalAttempt*10) * time.Second)
	}
	if completeErr != nil {
		// Save NZB locally as last resort so it's not lost.
		backupPath := filepath.Join(cfg.TempDir, fmt.Sprintf("backup-request-%d.nzb", task.RequestID))
		if err := os.WriteFile(backupPath, nzbData, 0644); err == nil {
			log.Printf("[%d] NZB saved to %s — upload manually if needed", rid, backupPath)
			site.PostLog("error", fmt.Sprintf("Completion failed for request #%d. NZB saved locally at %s", task.RequestID, backupPath))
		}
		reportProgress("Failed", "Report error: "+completeErr.Error())
		return
	}

	log.Printf("[%d] ── Pipeline complete: %s (total %s) ──", rid, task.Title, time.Since(taskStart).Round(time.Second))
	storage.UpdateState(jobName, "Completed", "Uploaded and reported to site.", 100)
}
