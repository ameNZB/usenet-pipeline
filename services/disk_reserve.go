package services

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ameNZB/usenet-pipeline/storage"
)

// diskReserved tracks the total bytes reserved by all in-flight tasks.
// Each task reserves space after learning its torrent size and releases
// it on completion (or failure). The polling loop checks
// FreeDiskAfterReservations() before accepting new work.
var diskReserved int64

// reservationsMu protects the per-task map for logging/debugging.
var reservationsMu sync.Mutex
var reservations = map[string]int64{} // jobName → bytes reserved

// ReserveDisk claims bytes for a task. Call once after the torrent size
// is known. The multiplier accounts for download + staging + PAR2:
//
//	downloaded file(s)         = 1.0x
//	staging copy (or hardlink) = ~0x if same device, 1.0x worst case
//	PAR2 recovery (5%)         = 0.05x
//	safety margin              = 0.05x
//
// Total: ~2.1x the torrent size covers the worst case.
const DiskMultiplier = 2.1

func ReserveDisk(jobName string, torrentBytes int64) {
	reserve := int64(float64(torrentBytes) * DiskMultiplier)
	atomic.AddInt64(&diskReserved, reserve)

	reservationsMu.Lock()
	reservations[jobName] = reserve
	reservationsMu.Unlock()

	log.Printf("Disk: reserved %.1f GB for %s (torrent=%.1f GB, total reserved=%.1f GB)",
		float64(reserve)/1e9, jobName, float64(torrentBytes)/1e9,
		float64(atomic.LoadInt64(&diskReserved))/1e9)
}

// ReleaseDisk frees the reservation for a completed/failed task.
func ReleaseDisk(jobName string) {
	reservationsMu.Lock()
	reserve, ok := reservations[jobName]
	if ok {
		delete(reservations, jobName)
	}
	reservationsMu.Unlock()

	if ok {
		atomic.AddInt64(&diskReserved, -reserve)
		log.Printf("Disk: released %.1f GB for %s (total reserved=%.1f GB)",
			float64(reserve)/1e9, jobName,
			float64(atomic.LoadInt64(&diskReserved))/1e9)
	}
}

// diskMaxBytes is the user-configured cap (0 = no cap, use all available).
// Set once at startup via InitDiskLimit.
var diskMaxBytes uint64

// InitDiskLimit sets the maximum disk usage. Call once at startup.
// maxGB=0 means no limit.
func InitDiskLimit(maxGB float64) {
	if maxGB > 0 {
		diskMaxBytes = uint64(maxGB * 1024 * 1024 * 1024)
		log.Printf("Disk: usage capped at %.0f GB", maxGB)
	}
}

// FreeDiskAfterReservations returns the effective free space: the lesser of
// actual free space and the user-configured budget, minus what's already
// reserved by in-flight tasks.
func FreeDiskAfterReservations(path string) (uint64, error) {
	free, err := FreeDiskSpace(path)
	if err != nil {
		return 0, err
	}

	// If a max disk cap is set, compute remaining budget from actual usage.
	if diskMaxBytes > 0 {
		used := diskUsage(path)
		if used >= diskMaxBytes {
			free = 0
		} else if remaining := diskMaxBytes - used; remaining < free {
			free = remaining
		}
	}

	reserved := atomic.LoadInt64(&diskReserved)
	if reserved <= 0 {
		return free, nil
	}
	if int64(free) <= reserved {
		return 0, nil
	}
	return free - uint64(reserved), nil
}

// diskUsage walks path and sums file sizes (how much the agent is currently using).
func diskUsage(path string) uint64 {
	var total uint64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

// TotalReservedBytes returns the current total reservation for logging.
func TotalReservedBytes() int64 {
	return atomic.LoadInt64(&diskReserved)
}

// SweepOrphanDownloads removes stale dl-* entries in tempDir left behind by
// crashed, aborted, or force-killed tasks. Called once at startup after
// storage.LoadState so the in-memory job map is populated.
//
// A dl-* directory is preserved only if some job state still references it
// as a DownloadedPath — that's the exact condition the resume path in
// processTask checks, so anything not in that set can never be resumed
// and is just wasted disk.
//
// The per-download outer dir TempDir/dl-{jobName}/ never gets cleaned by
// processTask's defer (which only removes the inner DownloadedPath under it),
// so even successful downloads leak the outer shell. The sweep catches those
// too.
func SweepOrphanDownloads(tempDir string) {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return
	}

	keep := map[string]bool{}
	storage.GlobalState.RLock()
	for _, job := range storage.GlobalState.Jobs {
		if job == nil || job.DownloadedPath == "" {
			continue
		}
		// DownloadedPath is TempDir/dl-{jobName}/{torrent-name}; keep the
		// parent directory so the resume logic can stat it.
		keep[filepath.Base(filepath.Dir(job.DownloadedPath))] = true
	}
	storage.GlobalState.RUnlock()

	var removed int
	var freedBytes uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "dl-") {
			continue
		}
		if keep[name] {
			continue
		}
		full := filepath.Join(tempDir, name)
		if e.IsDir() {
			freedBytes += diskUsage(full)
		} else if info, err := e.Info(); err == nil {
			freedBytes += uint64(info.Size())
		}
		if err := os.RemoveAll(full); err != nil {
			log.Printf("Sweep: failed to remove %s: %v", full, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Printf("Sweep: removed %d orphan dl-* entries (%.1f GB freed)",
			removed, float64(freedBytes)/1e9)
	}
}
