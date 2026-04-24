package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
	"github.com/ameNZB/usenet-pipeline/utils"

	"github.com/anacrolix/torrent"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
)

// ErrInsufficientDisk is returned by the pre-flight capacity check when a
// specific torrent won't fit on this agent. Callers use errors.Is to detect
// this case and skip the self-pause counter — one oversized torrent
// shouldn't pause the whole queue when smaller ones could still succeed.
var ErrInsufficientDisk = errors.New("insufficient disk space")

// DiskShortfallError wraps ErrInsufficientDisk with the discovered torrent
// size so the abort path can report it back to the site. Once the site
// stores size_bytes on the request, the poll dispatcher will filter this
// torrent out for every agent (and every future poll from this one) with
// equal-or-less free disk, eliminating the repeat metadata-fetch cost for
// the oversize backlog. errors.Is(err, ErrInsufficientDisk) keeps working
// via Unwrap; errors.As gives access to the fields.
type DiskShortfallError struct {
	TorrentBytes    int64 // total_length the agent just learned
	AvailableBytes  int64 // what we had free after reservations
}

func (e *DiskShortfallError) Error() string {
	return fmt.Sprintf("insufficient disk space: torrent is %.1f GB, have %.1f GB free",
		float64(e.TorrentBytes)/1e9, float64(e.AvailableBytes)/1e9)
}

func (e *DiskShortfallError) Unwrap() error { return ErrInsufficientDisk }

// layeredInt pulls a whole-number setting from the layered config; returns
// the fallback when the key is unset or not parseable.
func layeredInt(cfg *config.Config, key string, fallback int) int {
	if cfg == nil || cfg.Layered == nil {
		return fallback
	}
	v := cfg.Layered.Effective(key)
	if v == "" {
		return fallback
	}
	if i, err := strconv.Atoi(v); err == nil {
		return i
	}
	return fallback
}

// layeredFloat is the float64 variant of layeredInt.
func layeredFloat(cfg *config.Config, key string, fallback float64) float64 {
	if cfg == nil || cfg.Layered == nil {
		return fallback
	}
	v := cfg.Layered.Effective(key)
	if v == "" {
		return fallback
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return fallback
}

// seedOpts reads torrent_* knobs from the layered config. 0 means "don't
// seed" and skips the whole seeding phase — matching the pre-seeding
// behaviour so users who haven't enabled it see no change.
type seedOpts struct {
	UploadKBps   int
	RatioTarget  float64
	MaxHours     float64
	RequireFull  bool
	StallMins    int
	ListenPort   int
}

func seedOptsFromConfig(cfg *config.Config) seedOpts {
	return seedOpts{
		UploadKBps:  layeredInt(cfg, "torrent_max_upload_kbps", 0),
		RatioTarget: layeredFloat(cfg, "torrent_seed_ratio", 0),
		MaxHours:    layeredFloat(cfg, "torrent_seed_hours", 0),
		RequireFull: layeredInt(cfg, "torrent_require_full_seed", 0) == 1,
		StallMins:   layeredInt(cfg, "torrent_no_full_seed_timeout_mins", 0),
		ListenPort:  layeredInt(cfg, "torrent_port", 0),
	}
}

// applyVPNProxy configures the torrent client to route all traffic through a
// SOCKS5 proxy (e.g. gluetun) when VPN_DOWNLOAD_ONLY is enabled. This lets
// torrent downloads go through the VPN while NNTP uploads go direct.
func applyVPNProxy(clientConfig *torrent.ClientConfig, cfg *config.Config) {
	if !cfg.VPNDownloadOnly || cfg.VPNProxyAddr == "" {
		return
	}
	dialer, err := proxy.SOCKS5("tcp", cfg.VPNProxyAddr, nil, proxy.Direct)
	if err != nil {
		log.Printf("WARNING: failed to create SOCKS5 dialer (%s): %v — downloads will NOT go through VPN", cfg.VPNProxyAddr, err)
		return
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		log.Printf("WARNING: SOCKS5 dialer does not support DialContext — downloads will NOT go through VPN")
		return
	}
	dialFn := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return ctxDialer.DialContext(ctx, network, addr)
	}
	clientConfig.TrackerDialContext = dialFn
	clientConfig.HTTPDialContext = dialFn
	proxyURL, _ := url.Parse("socks5://" + cfg.VPNProxyAddr)
	clientConfig.HTTPProxy = http.ProxyURL(proxyURL)
	log.Printf("VPN split-tunnel: torrent traffic routed via SOCKS5 proxy %s", cfg.VPNProxyAddr)
}

// newTorrentClient creates a torrent.Client, and if the first attempt fails
// with a "port already in use" error (either because torrent_port was pinned
// to a busy port or a previous client's socket is still in TIME_WAIT), it
// retries once with ListenPort=0 so the OS picks a fresh random port. Every
// other error is returned unchanged.
func newTorrentClient(clientConfig *torrent.ClientConfig) (*torrent.Client, error) {
	client, err := torrent.NewClient(clientConfig)
	if err == nil {
		return client, nil
	}
	if !isBindInUseError(err) {
		return nil, err
	}
	log.Printf("torrent client bind failed on port %d (%v) — retrying with random port", clientConfig.ListenPort, err)
	clientConfig.ListenPort = 0
	return torrent.NewClient(clientConfig)
}

// isBindInUseError returns true if err looks like an "address already in use"
// bind failure from the torrent library's listener setup. Match is on string
// substrings because the library wraps the underlying OS error several levels
// deep ("first listen: listen tcp4 :NNNN: bind: address already in use").
func isBindInUseError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "Only one usage of each socket address")
}

// DownloadPrivateTorrentBytes runs the .torrent-file download path against a
// blob the site handed us (from a private upload). The bytes are staged to
// a temp file so we can reuse the existing AddTorrentFromFile pipeline, and
// we force DHT off on the client so the info hash never leaves the private
// tracker's swarm even if the .torrent forgot to set info.private = 1.
func DownloadPrivateTorrentBytes(ctx context.Context, torrentBytes []byte, cfg *config.Config, jobName string, opts *DownloadOpts) (string, error) {
	tempPath := filepath.Join(cfg.TempDir, "dl-"+jobName+".torrent")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.WriteFile(tempPath, torrentBytes, 0644); err != nil {
		return "", fmt.Errorf("stage .torrent file: %w", err)
	}
	defer os.Remove(tempPath)
	return downloadTorrentFile(ctx, tempPath, cfg, jobName, true)
}

// DownloadCachedTorrentBytes is the public-torrent equivalent of
// DownloadPrivateTorrentBytes. Used when the site's metadata-prefetch
// worker has already resolved the .torrent for a public request, so
// the agent can skip its own 2-minute DHT round-trip. DHT stays on
// (peers come from DHT + the trackers baked into the .torrent), and
// no public-tracker injection happens — those trackers are already in
// the file.
func DownloadCachedTorrentBytes(ctx context.Context, torrentBytes []byte, cfg *config.Config, jobName string, opts *DownloadOpts) (string, error) {
	tempPath := filepath.Join(cfg.TempDir, "dl-"+jobName+".torrent")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.WriteFile(tempPath, torrentBytes, 0644); err != nil {
		return "", fmt.Errorf("stage .torrent file: %w", err)
	}
	defer os.Remove(tempPath)
	return downloadTorrentFile(ctx, tempPath, cfg, jobName, false)
}

// DownloadTorrent handles adding a torrent file and downloading its contents.
func DownloadTorrent(ctx context.Context, torrentPath string, cfg *config.Config, jobName string) (string, error) {
	return downloadTorrentFile(ctx, torrentPath, cfg, jobName, false)
}

func downloadTorrentFile(ctx context.Context, torrentPath string, cfg *config.Config, jobName string, privateMode bool) (string, error) {
	dataDir := filepath.Join(cfg.TempDir, "dl-"+jobName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}

	so := seedOptsFromConfig(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = dataDir
	clientConfig.DisableIPv6 = true
	clientConfig.NoDefaultPortForwarding = true
	clientConfig.ListenPort = so.ListenPort // 0 = random
	if privateMode {
		// Private-tracker torrents must not leak their info hash to DHT
		// or trade peers over PEX/LSD. anacrolix honors info.private=1 in
		// the .torrent automatically, but we force it here too as a
		// belt-and-suspenders — even a mis-flagged .torrent stays
		// contained to the private tracker's swarm.
		clientConfig.NoDHT = true
	}
	if so.UploadKBps > 0 {
		// burst = 1s worth of tokens keeps the limiter responsive without
		// starving bursty writers. Values are bytes/sec, not bits.
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(so.UploadKBps*1024), so.UploadKBps*1024)
	}
	applyVPNProxy(clientConfig, cfg)

	client, err := newTorrentClient(clientConfig)
	if err != nil {
		return "", err
	}
	defer client.Close()

	t, err := client.AddTorrentFromFile(torrentPath)
	if err != nil {
		return "", err
	}

	log.Printf("Fetching metadata for %s...", filepath.Base(torrentPath))
	<-t.GotInfo()

	return downloadAndWaitSeed(ctx, client, t, dataDir, jobName, nil, so)
}

// DownloadMagnet downloads a torrent by magnet URI (used for site-assigned tasks).
// publicTrackers are appended to magnet URIs to improve metadata resolution.
var publicTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"http://nyaa.tracker.wf:7777/announce",
}

func DownloadMagnet(ctx context.Context, magnetURI string, cfg *config.Config, jobName string, opts *DownloadOpts) (string, error) {
	return downloadMagnet(ctx, magnetURI, cfg, jobName, opts, false)
}

// DownloadPrivateMagnet is like DownloadMagnet but skips the public-tracker
// injection — required for private-tracker torrents where announcing to the
// public trackers would leak the release to strangers and risk the user's
// tracker account. Private .torrent files already carry their own (private)
// announce list; we use that alone.
func DownloadPrivateMagnet(ctx context.Context, magnetURI string, cfg *config.Config, jobName string, opts *DownloadOpts) (string, error) {
	return downloadMagnet(ctx, magnetURI, cfg, jobName, opts, true)
}

func downloadMagnet(ctx context.Context, magnetURI string, cfg *config.Config, jobName string, opts *DownloadOpts, privateMode bool) (string, error) {
	// Append public trackers if not already present — but never for private
	// torrents, where announcing to the public trackers would de-anonymize
	// the user's private-tracker traffic.
	if !privateMode {
		for _, tr := range publicTrackers {
			if !strings.Contains(magnetURI, tr) {
				magnetURI += "&tr=" + url.QueryEscape(tr)
			}
		}
	}

	// Each download gets its own data dir to avoid piece-completion DB conflicts
	// when multiple torrent clients run concurrently.
	dataDir := filepath.Join(cfg.TempDir, "dl-"+jobName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}

	so := seedOptsFromConfig(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = dataDir
	clientConfig.DisableIPv6 = true
	clientConfig.NoDefaultPortForwarding = true
	clientConfig.ListenPort = so.ListenPort // 0 = random
	if so.UploadKBps > 0 {
		// burst = 1s worth of tokens keeps the limiter responsive without
		// starving bursty writers. Values are bytes/sec, not bits.
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(so.UploadKBps*1024), so.UploadKBps*1024)
	}
	applyVPNProxy(clientConfig, cfg)

	client, err := newTorrentClient(clientConfig)
	if err != nil {
		return "", err
	}
	defer client.Close()

	t, err := client.AddMagnet(magnetURI)
	if err != nil {
		return "", err
	}

	log.Printf("Fetching metadata for magnet (timeout 2min)...")
	metaTimeout := time.After(2 * time.Minute)
	select {
	case <-t.GotInfo():
		log.Printf("Metadata received: %s (%d bytes)", t.Name(), t.Info().TotalLength())
	case <-metaTimeout:
		t.Drop()
		return "", fmt.Errorf("metadata fetch timed out after 2 minutes (DHT/trackers unreachable)")
	case <-ctx.Done():
		t.Drop()
		return "", ctx.Err()
	}

	// Check disk space accounting for other in-flight tasks' reservations.
	torrentSize := t.Info().TotalLength()
	requiredBytes := int64(float64(torrentSize) * DiskMultiplier)
	effective, err := FreeDiskAfterReservations(cfg.TempDir)
	if err != nil {
		log.Printf("Warning: could not check disk space: %v", err)
	} else if effective < uint64(requiredBytes) {
		t.Drop()
		return "", &DiskShortfallError{
			TorrentBytes:   torrentSize,
			AvailableBytes: int64(effective),
		}
	} else {
		log.Printf("Disk space OK: %.1f GB effective free, reserving %.1f GB",
			float64(effective)/1e9, float64(requiredBytes)/1e9)
	}

	// Reserve space NOW (before download starts) so concurrent tasks see it.
	ReserveDisk(jobName, torrentSize)

	return downloadAndWaitSeed(ctx, client, t, dataDir, jobName, opts, so)
}

// runSeedPhase keeps the torrent active after download completion until
// one of these boundary conditions is hit: target upload ratio reached,
// max seed time elapsed, or the context is cancelled. The UploadRateLimiter
// configured on the client bounds outbound bandwidth; here we only track
// progress and emit status updates the dashboard renders as a seed bar.
//
// Ratio is computed as bytesWritten / torrentSize — close enough for the
// display and the stopping condition. anacrolix/torrent doesn't expose a
// first-class ratio getter, so this stays manual.
func runSeedPhase(ctx context.Context, t *torrent.Torrent, jobName string, so seedOpts) {
	total := t.Length()
	if total <= 0 {
		return
	}
	deadline := time.Time{}
	if so.MaxHours > 0 {
		deadline = time.Now().Add(time.Duration(so.MaxHours * float64(time.Hour)))
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	log.Printf("[%s] Seeding: ratio target %.2f, max %.1fh, cap %d KB/s",
		jobName, so.RatioTarget, so.MaxHours, so.UploadKBps)
	var lastUp, lastDown int64
	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] Seeding: cancelled", jobName)
			return
		case <-ticker.C:
			stats := t.Stats()
			uploaded := stats.BytesWrittenData.Int64()
			downloaded := stats.BytesReadData.Int64()
			ratio := float64(uploaded) / float64(total)
			upSpeed := float64(uploaded-lastUp) / 2.0 / 1024 / 1024   // MB/s (2s tick)
			dnSpeed := float64(downloaded-lastDown) / 2.0 / 1024 / 1024
			lastUp = uploaded
			lastDown = downloaded
			peers := len(t.PeerConns())

			// Surface seed progress through the existing callback so the
			// dashboard can render a ratio/time bar without a new channel.
			if cb := GetProgressCallback(jobName); cb != nil {
				// Percent here is seed progress (ratio%) — the "phase"
				// value tells the dashboard to switch to a seed bar.
				var pct float64
				if so.RatioTarget > 0 {
					pct = ratio / so.RatioTarget * 100
				} else if !deadline.IsZero() {
					total := deadline.Sub(deadline.Add(-time.Duration(so.MaxHours * float64(time.Hour))))
					elapsed := time.Since(deadline.Add(-time.Duration(so.MaxHours * float64(time.Hour))))
					pct = float64(elapsed) / float64(total) * 100
				}
				if pct > 100 {
					pct = 100
				}
				// Total/transferred during seeding: keep the original
				// torrent size as "total" and bytes-uploaded so far as
				// "transferred" so the dashboard can show "uploaded
				// X / Y" instead of a bare ratio. ETA is ratio- or
				// time-bounded, not byte-bounded, so leave it as 0.
				cb(dnSpeed, upSpeed, pct, "seeding", peers, t.Length(), uploaded, 0, nil)
			}
			storage.UpdateState(jobName, "Seeding",
				fmt.Sprintf("ratio %.3f / %.2f - %.2f MB/s up - %d peers", ratio, so.RatioTarget, upSpeed, peers),
				0)

			if so.RatioTarget > 0 && ratio >= so.RatioTarget {
				log.Printf("[%s] Seeding: ratio target %.2f reached (%.3f)", jobName, so.RatioTarget, ratio)
				return
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				log.Printf("[%s] Seeding: max time %.1fh elapsed (ratio %.3f)", jobName, so.MaxHours, ratio)
				return
			}
		}
	}
}

// ProgressWarning is one rule-violation countdown surfaced to the site
// so the admin / owner dashboards can render a live "will skip in X"
// indicator. ExpiresAt is absolute (now + remaining timeout), so the
// browser ticks it down without extra polling.
type ProgressWarning struct {
	Kind      string    // slow_speed | low_peers | stalled
	Label     string    // hover description
	Icon      string    // emoji shown next to the timer
	ExpiresAt time.Time // when the rule will trigger a skip
}

// ProgressCallback is called periodically with the running stats for a
// task. downMBs is the payload receive rate, upMBs is the payload send
// rate (peer uploads in a torrent phase, NNTP upload rate in the usenet
// phase). totalBytes / transferredBytes are the absolute counters so
// the dashboard can render "X / Y MB" without re-deriving from percent
// (which loses precision and bottoms out at 100). etaSeconds is the
// remaining time at the current speed; 0 means unknown / not applicable.
// warnings is the current active-rule countdown set; empty means the
// task is healthy.
type ProgressCallback func(downMBs float64, upMBs float64, percent float64, phase string, peers int, totalBytes int64, transferredBytes int64, etaSeconds float64, warnings []ProgressWarning)

// ErrSlowDownload is returned when a download is rejected for being too slow.
var ErrSlowDownload = fmt.Errorf("download rejected: speed too low for too long")

// DownloadOpts holds optional parameters for the download loop.
type DownloadOpts struct {
	SlowThresholdMBs    float64 // speed below this is "slow" (0 = no limit)
	SlowTimeoutMins     int     // minutes of sustained slow speed before rejection
	LowPeersThreshold   int     // skip if seeds <= this (-1 = disabled)
	LowPeersTimeoutMins int     // minutes of sustained low seeds before rejection
	IsBoosted           bool    // boosted requests tolerate slow (non-zero) speeds
}

// downloadAndWait runs the download loop with progress reporting.
func downloadAndWait(ctx context.Context, cl *torrent.Client, t *torrent.Torrent, dataDir string, jobName string, opts *DownloadOpts) (string, error) {
	return downloadAndWaitSeed(ctx, cl, t, dataDir, jobName, opts, seedOpts{})
}

// downloadAndWaitSeed is the variant that also applies the torrent_*
// layered settings: pre-start full-seed gate, zero-progress stall timeout,
// and a post-download seeding phase bounded by ratio and/or hours.
func downloadAndWaitSeed(ctx context.Context, cl *torrent.Client, t *torrent.Torrent, dataDir string, jobName string, opts *DownloadOpts, so seedOpts) (string, error) {
	log.Printf("Downloading %s (%d bytes)...", t.Name(), t.Info().TotalLength())
	t.DownloadAll()

	done := make(chan struct{})
	go func() {
		cl.WaitAll()
		close(done)
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	total := t.Length()
	var lastCompleted int64
	var lastUploaded int64
	var lastLog time.Time
	startedAt := time.Now()
	var stallSince time.Time

	// Slow download tracking.
	var slowSince time.Time
	slowTimeout := time.Duration(0)
	slowThreshold := 0.0
	isBoosted := false
	// Low peer tracking.
	var lowPeersSince time.Time
	lowPeersTimeout := time.Duration(0)
	lowPeersThreshold := -1 // -1 = disabled

	// Hysteresis: require N consecutive good ticks before clearing a
	// rule's "since" timer. Without this the dashboard warning
	// flickers as speed/peers bounce around the threshold every tick.
	const recoveryTicksRequired = 3
	var slowRecovery, stallRecovery, lowPeersRecovery int
	if opts != nil {
		slowThreshold = opts.SlowThresholdMBs
		if opts.SlowTimeoutMins > 0 {
			slowTimeout = time.Duration(opts.SlowTimeoutMins) * time.Minute
		}
		isBoosted = opts.IsBoosted
		lowPeersThreshold = opts.LowPeersThreshold
		if opts.LowPeersTimeoutMins > 0 {
			lowPeersTimeout = time.Duration(opts.LowPeersTimeoutMins) * time.Minute
		}
	}

	for {
		select {
		case <-ctx.Done():
			t.Drop()
			return filepath.Join(dataDir, t.Name()), ctx.Err()
		case <-done:
			storage.UpdateState(jobName, "Downloading", "100% (Download Complete)", 100)
			if cb := GetProgressCallback(jobName); cb != nil {
				cb(0, 0, 100, "downloading", 0, t.Length(), t.Length(), 0, nil)
			}
			// Enter the optional seeding phase. UploadKBps == 0 keeps the
			// pre-existing behaviour of dropping the torrent immediately.
			if so.UploadKBps > 0 && (so.RatioTarget > 0 || so.MaxHours > 0) {
				runSeedPhase(ctx, t, jobName, so)
			}
			return filepath.Join(dataDir, t.Name()), nil
		case <-ticker.C:
			completed := t.BytesCompleted()
			peers := len(t.PeerConns())
			dlStats := t.Stats()
			uploaded := dlStats.BytesWrittenData.Int64()
			if total > 0 {
				percent := float64(completed) / float64(total) * 100
				speed := float64(completed-lastCompleted) / 1024 / 1024
				upSpeed := float64(uploaded-lastUploaded) / 1024 / 1024

				var etaSeconds float64
				etaStr := "Calculating..."
				if speed > 0 {
					remainingMB := float64(total-completed) / 1024 / 1024
					etaSeconds = remainingMB / speed
					etaStr = utils.FormatETA(etaSeconds)
				}

				lastCompleted = completed
				lastUploaded = uploaded
				storage.UpdateState(jobName, "Downloading", fmt.Sprintf("%.1f%% (%.2f / %.2f MB) - %.2f MB/s dn / %.2f MB/s up - ETA: %s - %d peers", percent, float64(completed)/1024/1024, float64(total)/1024/1024, speed, upSpeed, etaStr, peers), percent)

				// Periodic log so stdout isn't silent during long downloads.
				if time.Since(lastLog) >= 30*time.Second {
					lastLog = time.Now()
					log.Printf("[%s] %.1f%% (%.1f/%.1f GB) %.2f MB/s %d peers",
						jobName, percent,
						float64(completed)/1e9, float64(total)/1e9,
						speed, peers)
				}

				// Slow download detection — skip the first 30s to allow ramp-up.
				slowActive := false
				if slowTimeout > 0 && slowThreshold > 0 && percent < 95 {
					isSlow := speed < slowThreshold
					// Boosted requests are only rejected if speed is truly zero.
					if isBoosted {
						isSlow = speed == 0
					}

					if isSlow {
						slowRecovery = 0
						if slowSince.IsZero() {
							slowSince = time.Now()
						} else if time.Since(slowSince) > slowTimeout {
							log.Printf("[%s] Rejecting slow download: %.4f MB/s for %v (threshold: %.2f MB/s, boosted: %v)",
								jobName, speed, time.Since(slowSince).Round(time.Second), slowThreshold, isBoosted)
							t.Drop()
							return filepath.Join(dataDir, t.Name()), ErrSlowDownload
						}
						slowActive = true
					} else if !slowSince.IsZero() {
						// Hysteresis: only clear the timer after several
						// consecutive good ticks so the dashboard
						// countdown doesn't flicker on threshold edges.
						slowRecovery++
						if slowRecovery >= recoveryTicksRequired {
							slowSince = time.Time{}
							slowRecovery = 0
						} else {
							slowActive = true
						}
					}
				}

				// Full-seed gate + stall detection (torrent_* layered settings).
				// If RequireFull is set, we treat the first 60s with zero
				// progress as "no full peer reachable" and drop. Past that,
				// StallMins minutes of zero progress + zero speed drops too.
				if so.RequireFull && completed == 0 && time.Since(startedAt) > 60*time.Second {
					log.Printf("[%s] Rejecting: no full seed (0 bytes after 60s)", jobName)
					t.Drop()
					return filepath.Join(dataDir, t.Name()), ErrSlowDownload
				}
				// No percent gate: a torrent stuck at 99.x% with 0 peers
				// never finishes — the earlier `percent < 99` cap let those
				// hold a slot forever. StallMins itself is the knob.
				stallActive := false
				if so.StallMins > 0 {
					if speed == 0 {
						stallRecovery = 0
						if stallSince.IsZero() {
							stallSince = time.Now()
						} else if time.Since(stallSince) > time.Duration(so.StallMins)*time.Minute {
							log.Printf("[%s] Rejecting stalled download: 0 speed for %v at %.1f%%", jobName, time.Since(stallSince).Round(time.Second), percent)
							t.Drop()
							return filepath.Join(dataDir, t.Name()), ErrSlowDownload
						}
						stallActive = true
					} else if !stallSince.IsZero() {
						stallRecovery++
						if stallRecovery >= recoveryTicksRequired {
							stallSince = time.Time{}
							stallRecovery = 0
						} else {
							stallActive = true
						}
					}
				}

				// Low peer detection — skip if seeds stay at or below threshold.
				lowPeersActive := false
				if lowPeersTimeout > 0 && lowPeersThreshold >= 0 && percent < 95 {
					if peers <= lowPeersThreshold {
						lowPeersRecovery = 0
						if lowPeersSince.IsZero() {
							lowPeersSince = time.Now()
						} else if time.Since(lowPeersSince) > lowPeersTimeout {
							log.Printf("[%s] Rejecting low-seed download: %d peers for %v (threshold: %d)",
								jobName, peers, time.Since(lowPeersSince).Round(time.Second), lowPeersThreshold)
							t.Drop()
							return filepath.Join(dataDir, t.Name()), ErrSlowDownload
						}
						lowPeersActive = true
					} else if !lowPeersSince.IsZero() {
						lowPeersRecovery++
						if lowPeersRecovery >= recoveryTicksRequired {
							lowPeersSince = time.Time{}
							lowPeersRecovery = 0
						} else {
							lowPeersActive = true
						}
					}
				}

				// Build the live warnings list from whichever rules are
				// currently counting down. ExpiresAt is absolute so the
				// browser can tick the countdown without re-polling.
				var warnings []ProgressWarning
				if slowActive && !slowSince.IsZero() {
					warnings = append(warnings, ProgressWarning{
						Kind:      "slow_speed",
						Label:     fmt.Sprintf("Speed below %.2f MB/s — will skip", slowThreshold),
						Icon:      "🐢",
						ExpiresAt: slowSince.Add(slowTimeout),
					})
				}
				if stallActive && !stallSince.IsZero() {
					warnings = append(warnings, ProgressWarning{
						Kind:      "stalled",
						Label:     "Download stalled at 0 MB/s — will skip",
						Icon:      "⏸",
						ExpiresAt: stallSince.Add(time.Duration(so.StallMins) * time.Minute),
					})
				}
				if lowPeersActive && !lowPeersSince.IsZero() {
					warnings = append(warnings, ProgressWarning{
						Kind:      "low_peers",
						Label:     fmt.Sprintf("Peers ≤ %d — will skip", lowPeersThreshold),
						Icon:      "👥",
						ExpiresAt: lowPeersSince.Add(lowPeersTimeout),
					})
				}

				if cb := GetProgressCallback(jobName); cb != nil {
					cb(speed, upSpeed, percent, "downloading", peers, total, completed, etaSeconds, warnings)
				}
			}
		}
	}
}
