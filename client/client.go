package client

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"github.com/ameNZB/usenet-pipeline/config"
)

// AgentTask is the task payload returned by the site when polling.
type AgentTask struct {
	RequestID  int64  `json:"request_id"`
	LockID     int    `json:"lock_id"`
	Title      string `json:"title"`
	NyaaURL    string `json:"nyaa_url,omitempty"`
	InfoHash   string `json:"info_hash,omitempty"`
	Category   string `json:"category,omitempty"`
	Season     string `json:"season,omitempty"`
	Episodes   string `json:"episodes,omitempty"`
	BoostCount int    `json:"boost_count,omitempty"`
}

// SiteClient communicates with the indexer site via HTTP.
type SiteClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// New creates a SiteClient from config.
func New(cfg *config.Config) *SiteClient {
	return &SiteClient{
		baseURL: cfg.SiteURL,
		token:   cfg.AgentToken,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// PollResult holds the response from a poll request.
type PollResult struct {
	Task    *AgentTask
	Reason  string // why no task was returned (empty if task assigned)
	Command string // "stop" if agent should stop
}

// Poll checks the site for available work.
func (c *SiteClient) Poll() (*PollResult, error) {
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/poll", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return &PollResult{Reason: "no content (legacy 204)"}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — check your AGENT_TOKEN")
	}
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("BLOCKED: %s (approve new IP in Account Settings on the site)", body)
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		var m MaintenanceResponse
		if json.Unmarshal(body, &m) == nil && m.Maintenance {
			return nil, &MaintenanceError{Info: m}
		}
		return nil, fmt.Errorf("poll returned 503: %s", body)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("poll returned %d: %s", resp.StatusCode, body)
	}

	// Parse the response — could be a task or a reason/command.
	var raw struct {
		AgentTask
		Reason  string `json:"reason"`
		Command string `json:"command"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}

	if raw.Command != "" {
		return &PollResult{Command: raw.Command}, nil
	}
	if raw.Reason != "" {
		return &PollResult{Reason: raw.Reason}, nil
	}
	if raw.RequestID == 0 {
		return &PollResult{Reason: "empty response (no request_id)"}, nil
	}
	task := raw.AgentTask
	return &PollResult{Task: &task}, nil
}

// RemoteConfig holds server-side agent configuration fetched from the site.
// WebOverrides is the key/value map from the agent_config_web table — only
// keys present here are applied as the web tier on the agent's Layered
// config (so a missing key falls through to env/yml).
type RemoteConfig struct {
	MaxConcurrent      int     `json:"max_concurrent"` // 0 = use local default
	CpuMaxPercent      int     `json:"cpu_max_percent"`
	MaxDiskUsageGB     float64 `json:"max_disk_usage_gb"`    // 0 = no limit
	SlowSpeedThreshold float64 `json:"slow_speed_threshold"` // MB/s
	SlowSpeedTimeout   int     `json:"slow_speed_timeout"`   // minutes
	LowPeersThreshold  int     `json:"low_peers_threshold"`  // skip if seeds <= this
	LowPeersTimeout    int     `json:"low_peers_timeout"`    // minutes
	WebOverrides       map[string]string `json:"web_overrides,omitempty"`
}

// PostLocalConfig uploads the agent's local snapshot (yml + env values for
// the known tunable keys) so the settings UI can show state badges and
// compare against any web-override the user has set.
func (c *SiteClient) PostLocalConfig(report config.SettingsReport) error {
	body, _ := json.Marshal(report)
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/local-config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Directive is a queued instruction from the site. Currently only
// kind="write_config" with Payload.Updates (map[string]string) is defined.
type Directive struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// WriteConfigPayload is the decoded payload for kind="write_config".
type WriteConfigPayload struct {
	Updates map[string]string `json:"updates"`
}

// FetchDirectives returns any pending directives queued for this agent.
func (c *SiteClient) FetchDirectives() ([]Directive, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/agent/directives", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Site hasn't shipped the directives endpoint yet — treat as empty.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("directives returned %d", resp.StatusCode)
	}
	var out struct {
		Directives []Directive `json:"directives"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Directives, nil
}

// AckDirective reports the outcome of a directive back to the site so the
// row can be marked consumed. Err is empty on success.
func (c *SiteClient) AckDirective(id int64, errMsg string) error {
	body, _ := json.Marshal(map[string]interface{}{"id": id, "error": errMsg})
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/directives/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetConfig fetches the agent configuration from the site.
func (c *SiteClient) GetConfig() (*RemoteConfig, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/agent/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config returned %d", resp.StatusCode)
	}
	var cfg RemoteConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PostLog sends a log entry to the site for display on the agent dashboard.
func (c *SiteClient) PostLog(level, message string) error {
	body, _ := json.Marshal(map[string]string{"level": level, "message": message})
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/log", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ClearMyLocks tells the site to expire all active locks held by this agent.
// Called on startup to recover from crashes.
func (c *SiteClient) ClearMyLocks() (int, error) {
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/clear-locks", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result struct {
		Cleared int `json:"cleared"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Cleared, nil
}

// ReportProgress sends a progress update to the site.
func (c *SiteClient) ReportProgress(lockID int, progress, speed string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"lock_id":  lockID,
		"progress": progress,
		"speed":    speed,
	})
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/progress", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// AgentLiveStatus is the real-time status posted to the site every few seconds.
type AgentLiveStatus struct {
	Phase          string         `json:"phase"`
	VPNStatus      string         `json:"vpn_status"`
	PublicIP       string         `json:"public_ip"`
	DownloadSpeed  string         `json:"download_speed,omitempty"`
	UploadSpeed    string         `json:"upload_speed,omitempty"`
	Files          []FileProgress `json:"files,omitempty"`
	TaskTitle      string         `json:"task_title,omitempty"`
	RequestID      int64          `json:"request_id,omitempty"`
	DiskFreeGB     float64        `json:"disk_free_gb,omitempty"`
	DiskReservedGB float64        `json:"disk_reserved_gb,omitempty"`
}

// FileProgress tracks per-file download/upload progress.
type FileProgress struct {
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	Transferred int64   `json:"transferred"`
	Percent     float64 `json:"percent"`
	Speed       string  `json:"speed,omitempty"`
	Phase       string  `json:"phase"`
	Peers       int     `json:"peers,omitempty"`
}

// PostStatus sends the agent's live status to the site for dashboard display.
// StatusResponse holds the site's response to a status post, which may include commands.
type StatusResponse struct {
	OK              bool  `json:"ok"`
	CancelRequestID int64 `json:"cancel_request_id,omitempty"`
}

func (c *SiteClient) PostStatus(status AgentLiveStatus) (*StatusResponse, error) {
	body, _ := json.Marshal(status)
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/status", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var sr StatusResponse
	json.NewDecoder(resp.Body).Decode(&sr)
	return &sr, nil
}

// CompleteResult holds all data sent on task completion.
type CompleteResult struct {
	LockID      int
	RequestID   int64
	Status      string // "completed" or "failed"
	NzbData     []byte
	Password    string
	MediaInfo   interface{} // *services.VideoInfo — JSON-serialized
	Screenshots []string    // file paths to screenshot JPEGs
}

// maxUploadSize is the threshold (uncompressed) above which screenshots are
// sent individually instead of bundled with the completion request.
// Cloudflare free-tier caps uploads at 100 MB; we stay well under.
const maxUploadSize = 80 << 20 // 80 MB

type screenshotFile struct {
	index int
	path  string
	size  int64
}

// Complete notifies the site that a task is done (or failed).
// Uses multipart form to send NZB data, metadata JSON, and screenshot images.
// If bundling everything would exceed maxUploadSize, screenshots are sent
// individually after the initial completion call.
func (c *SiteClient) Complete(result CompleteResult) error {
	// Stat screenshot files up front.
	var screenshots []screenshotFile
	var ssTotal int64
	for i, path := range result.Screenshots {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		screenshots = append(screenshots, screenshotFile{index: i + 1, path: path, size: info.Size()})
		ssTotal += info.Size()
	}

	// Build the base form (no screenshots) to measure its size.
	baseBuf, baseCT := c.buildCompleteForm(result, nil)
	baseSize := int64(baseBuf.Len())

	if baseSize+ssTotal+int64(len(screenshots))*512 <= maxUploadSize {
		// Everything fits in one request.
		buf, ct := c.buildCompleteForm(result, screenshots)
		log.Printf("Reporting to site: %d bytes (with %d screenshots), gzipping...", buf.Len(), len(screenshots))
		resp, err := c.postGzipped(c.baseURL+"/api/agent/complete", buf.Bytes(), ct)
		if err != nil {
			log.Printf("Complete POST failed: %v", err)
		} else {
			log.Printf("Complete POST succeeded (response: %v)", resp)
		}
		return err
	}

	// Too large — send completion without screenshots, then upload each
	// screenshot individually using the NZB ID the site returns.
	log.Printf("Reporting to site: payload too large (%d base + %d screenshots = %d bytes) — splitting",
		baseSize, ssTotal, baseSize+ssTotal)
	resp, err := c.postGzipped(c.baseURL+"/api/agent/complete", baseBuf.Bytes(), baseCT)
	if err != nil {
		log.Printf("Complete POST failed: %v", err)
		return err
	}
	log.Printf("Complete POST succeeded (response: %v)", resp)
	var nzbID int64
	if v, ok := resp["nzb_id"]; ok {
		switch n := v.(type) {
		case float64:
			nzbID = int64(n)
		case json.Number:
			nzbID, _ = n.Int64()
		}
	}
	if nzbID == 0 {
		log.Printf("No nzb_id in response — cannot send screenshots separately")
		return nil
	}

	log.Printf("Sending %d screenshots individually for nzb_id=%d", len(screenshots), nzbID)
	for _, sf := range screenshots {
		if err := c.uploadScreenshot(nzbID, sf.index, sf.path); err != nil {
			log.Printf("WARN: screenshot %d upload failed: %v", sf.index, err)
		} else {
			log.Printf("Screenshot %d uploaded OK", sf.index)
		}
	}
	return nil
}

// buildCompleteForm constructs the multipart body for /api/agent/complete.
// Returns the body bytes and the Content-Type header (with boundary).
func (c *SiteClient) buildCompleteForm(result CompleteResult, screenshots []screenshotFile) (bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	w.WriteField("lock_id", fmt.Sprintf("%d", result.LockID))
	w.WriteField("request_id", fmt.Sprintf("%d", result.RequestID))
	w.WriteField("status", result.Status)
	if result.Password != "" {
		w.WriteField("password", result.Password)
	}

	if result.NzbData != nil {
		if part, err := w.CreateFormFile("nzb_data", "release.nzb"); err == nil {
			part.Write(result.NzbData)
		}
	}

	if result.MediaInfo != nil {
		if infoJSON, err := json.Marshal(result.MediaInfo); err == nil {
			w.WriteField("media_info", string(infoJSON))
		}
	}

	for _, sf := range screenshots {
		f, err := os.Open(sf.path)
		if err != nil {
			continue
		}
		if part, err := w.CreateFormFile(fmt.Sprintf("screenshot_%d", sf.index), filepath.Base(sf.path)); err == nil {
			io.Copy(part, f)
		}
		f.Close()
	}

	ct := w.FormDataContentType()
	w.Close()
	return buf, ct
}

// postGzipped gzip-compresses body and POSTs it with auth headers.
// Returns the parsed JSON response map on success.
func (c *SiteClient) postGzipped(url string, body []byte, contentType string) (map[string]interface{}, error) {
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(body); err != nil {
		return nil, fmt.Errorf("gzip compress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}

	req, err := http.NewRequest("POST", url, &gzBuf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Detect maintenance mode: the site returns 503 with a JSON body
		// like {"maintenance":true,"reason":"...","eta_seconds":243}.
		// Parse it into a typed error so the caller can wait for maintenance
		// to end instead of burning retries.
		if resp.StatusCode == http.StatusServiceUnavailable {
			var m MaintenanceResponse
			if json.Unmarshal(respBody, &m) == nil && m.Maintenance {
				return nil, &MaintenanceError{Info: m}
			}
		}
		return nil, fmt.Errorf("returned %d: %s", resp.StatusCode, respBody)
	}
	var result map[string]interface{}
	json.Unmarshal(respBody, &result)
	return result, nil
}

// MaintenanceResponse is the JSON payload the site returns with a 503 when
// it's in maintenance mode (e.g. VACUUM FULL, backup, migration).
type MaintenanceResponse struct {
	Maintenance    bool   `json:"maintenance"`
	Reason         string `json:"reason"`
	StartedAt      int64  `json:"started_at"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
	ETASeconds     int    `json:"eta_seconds"`
}

// MaintenanceError is returned by Complete/postGzipped when the site is in
// maintenance mode. Callers should wait (not retry at a fixed backoff) and
// can inspect Info.ETASeconds for a hint at how long.
type MaintenanceError struct {
	Info MaintenanceResponse
}

func (e *MaintenanceError) Error() string {
	return fmt.Sprintf("site maintenance: %s (elapsed %ds, eta %ds)",
		e.Info.Reason, e.Info.ElapsedSeconds, e.Info.ETASeconds)
}

// IsMaintenanceError reports whether err wraps a MaintenanceError.
func IsMaintenanceError(err error) (*MaintenanceError, bool) {
	var me *MaintenanceError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// uploadScreenshot sends a single screenshot to /api/agent/screenshot.
func (c *SiteClient) uploadScreenshot(nzbID int64, index int, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("nzb_id", fmt.Sprintf("%d", nzbID))
	if part, err := w.CreateFormFile("screenshot", filepath.Base(path)); err == nil {
		io.Copy(part, f)
	}
	ct := w.FormDataContentType()
	w.Close()

	_, err = c.postGzipped(c.baseURL+"/api/agent/screenshot", buf.Bytes(), ct)
	return err
}
