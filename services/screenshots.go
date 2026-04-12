package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
)

// screenshotWorkers controls how many ffmpeg screenshot processes run in
// parallel. 3 is a good balance — input seeking is largely I/O-bound (disk
// read) so more workers overlap the seek latency without hammering the CPU.
const screenshotWorkers = 3

// GenerateScreenshots creates lossless PNG screenshots at evenly spaced
// intervals through the video. We emit PNG (rather than JPEG) so the server
// can re-encode to lossless WebP without ever going through a lossy stage —
// users use these to judge release quality, and any JPEG step in the middle
// would defeat that. Returns the file paths of generated screenshots.
// count is the number of screenshots (default 6).
func GenerateScreenshots(ctx context.Context, videoPath, outputDir string, duration float64, count int) ([]string, error) {
	if count <= 0 {
		count = 6
	}
	if duration <= 0 {
		return nil, fmt.Errorf("invalid duration: %.1f", duration)
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	// Skip the first and last 5% to avoid intros/credits/black frames.
	start := duration * 0.05
	end := duration * 0.95
	span := end - start
	if span <= 0 {
		span = duration
		start = 0
	}

	// Capture screenshots in parallel — ffmpeg seeking is I/O-bound, so
	// overlapping seeks cuts wall-clock time roughly in half for 6 shots.
	type result struct {
		index int
		path  string
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
		sem     = make(chan struct{}, screenshotWorkers)
	)

	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}        // acquire worker slot
			defer func() { <-sem }() // release

			if ctx.Err() != nil {
				return
			}

			t := start + span*float64(idx)/float64(count)
			timestamp := formatTimestamp(t)
			outPath := filepath.Join(outputDir, fmt.Sprintf("screen_%02d.png", idx+1))

			cmd := exec.CommandContext(ctx, "ffmpeg",
				"-ss", timestamp,
				"-i", videoPath,
				"-vframes", "1",
				"-c:v", "png",
				"-y",
				outPath,
			)
			cmd.Stdout = nil
			cmd.Stderr = nil

			if err := cmd.Run(); err != nil {
				return
			}

			if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
				mu.Lock()
				results = append(results, result{index: idx, path: outPath})
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("no screenshots generated")
	}

	// Return paths in index order so screenshots appear in timeline order.
	sort.Slice(results, func(a, b int) bool { return results[a].index < results[b].index })
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.path)
	}
	return paths, nil
}

// formatTimestamp converts seconds to HH:MM:SS.mmm format for ffmpeg.
func formatTimestamp(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := seconds - float64(h*3600+m*60)
	return fmt.Sprintf("%02d:%02d:%06.3f", h, m, s)
}
