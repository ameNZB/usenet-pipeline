package services

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
	"github.com/ameNZB/usenet-pipeline/utils"

	"github.com/anacrolix/torrent"
	"golang.org/x/net/proxy"
)

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

// DownloadTorrent handles adding a torrent file and downloading its contents.
func DownloadTorrent(ctx context.Context, torrentPath string, cfg *config.Config, jobName string) (string, error) {
	dataDir := filepath.Join(cfg.TempDir, "dl-"+jobName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}

	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = dataDir
	clientConfig.DisableIPv6 = true
	clientConfig.NoDefaultPortForwarding = true
	clientConfig.ListenPort = 0 // random port
	applyVPNProxy(clientConfig, cfg)

	client, err := torrent.NewClient(clientConfig)
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

	return downloadAndWait(ctx, client, t, dataDir, jobName, nil)
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
	// Append public trackers if not already present.
	for _, tr := range publicTrackers {
		if !strings.Contains(magnetURI, tr) {
			magnetURI += "&tr=" + url.QueryEscape(tr)
		}
	}

	// Each download gets its own data dir to avoid piece-completion DB conflicts
	// when multiple torrent clients run concurrently.
	dataDir := filepath.Join(cfg.TempDir, "dl-"+jobName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}

	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = dataDir
	clientConfig.DisableIPv6 = true
	clientConfig.NoDefaultPortForwarding = true
	clientConfig.ListenPort = 0 // random port — avoids bind conflicts with concurrent downloads
	applyVPNProxy(clientConfig, cfg)

	client, err := torrent.NewClient(clientConfig)
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
		return "", fmt.Errorf("insufficient disk space: need %.1f GB (%.1fx %.1f GB torrent), have %.1f GB effective free",
			float64(requiredBytes)/1e9, DiskMultiplier,
			float64(torrentSize)/1e9,
			float64(effective)/1e9)
	} else {
		log.Printf("Disk space OK: %.1f GB effective free, reserving %.1f GB",
			float64(effective)/1e9, float64(requiredBytes)/1e9)
	}

	// Reserve space NOW (before download starts) so concurrent tasks see it.
	ReserveDisk(jobName, torrentSize)

	return downloadAndWait(ctx, client, t, dataDir, jobName, opts)
}

// ProgressCallback is called periodically with speed (MB/s), percent (0-100), and peer count.
type ProgressCallback func(speedMBs float64, percent float64, phase string, peers int)

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
	var lastLog time.Time

	// Slow download tracking.
	var slowSince time.Time
	slowTimeout := time.Duration(0)
	slowThreshold := 0.0
	isBoosted := false
	// Low peer tracking.
	var lowPeersSince time.Time
	lowPeersTimeout := time.Duration(0)
	lowPeersThreshold := -1 // -1 = disabled
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
				cb(0, 100, "downloading", 0)
			}
			return filepath.Join(dataDir, t.Name()), nil
		case <-ticker.C:
			completed := t.BytesCompleted()
			peers := len(t.PeerConns())
			if total > 0 {
				percent := float64(completed) / float64(total) * 100
				speed := float64(completed-lastCompleted) / 1024 / 1024

				etaStr := "Calculating..."
				if speed > 0 {
					remainingMB := float64(total-completed) / 1024 / 1024
					etaSeconds := remainingMB / speed
					etaStr = utils.FormatETA(etaSeconds)
				}

				lastCompleted = completed
				storage.UpdateState(jobName, "Downloading", fmt.Sprintf("%.1f%% (%.2f / %.2f MB) - %.2f MB/s - ETA: %s - %d peers", percent, float64(completed)/1024/1024, float64(total)/1024/1024, speed, etaStr, peers), percent)

				if cb := GetProgressCallback(jobName); cb != nil {
					cb(speed, percent, "downloading", peers)
				}

				// Periodic log so stdout isn't silent during long downloads.
				if time.Since(lastLog) >= 30*time.Second {
					lastLog = time.Now()
					log.Printf("[%s] %.1f%% (%.1f/%.1f GB) %.2f MB/s %d peers",
						jobName, percent,
						float64(completed)/1e9, float64(total)/1e9,
						speed, peers)
				}

				// Slow download detection — skip the first 30s to allow ramp-up.
				if slowTimeout > 0 && slowThreshold > 0 && percent < 95 {
					isSlow := speed < slowThreshold
					// Boosted requests are only rejected if speed is truly zero.
					if isBoosted {
						isSlow = speed == 0
					}

					if isSlow {
						if slowSince.IsZero() {
							slowSince = time.Now()
						} else if time.Since(slowSince) > slowTimeout {
							log.Printf("[%s] Rejecting slow download: %.4f MB/s for %v (threshold: %.2f MB/s, boosted: %v)",
								jobName, speed, time.Since(slowSince).Round(time.Second), slowThreshold, isBoosted)
							t.Drop()
							return filepath.Join(dataDir, t.Name()), ErrSlowDownload
						}
					} else {
						slowSince = time.Time{} // reset timer when speed recovers
					}
				}

				// Low peer detection — skip if seeds stay at or below threshold.
				if lowPeersTimeout > 0 && lowPeersThreshold >= 0 && percent < 95 {
					if peers <= lowPeersThreshold {
						if lowPeersSince.IsZero() {
							lowPeersSince = time.Now()
						} else if time.Since(lowPeersSince) > lowPeersTimeout {
							log.Printf("[%s] Rejecting low-seed download: %d peers for %v (threshold: %d)",
								jobName, peers, time.Since(lowPeersSince).Round(time.Second), lowPeersThreshold)
							t.Drop()
							return filepath.Join(dataDir, t.Name()), ErrSlowDownload
						}
					} else {
						lowPeersSince = time.Time{}
					}
				}
			}
		}
	}
}
