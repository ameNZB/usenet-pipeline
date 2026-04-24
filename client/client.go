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
	"strings"
	"sync/atomic"
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

	// Private tells the agent to fetch the .torrent file from
	// TorrentFileURL (a site-relative path) instead of resolving the info
	// hash via magnet/DHT. Set by the site when the uploading user marked
	// the request as private — the agent must also skip any public-tracker
	// injection to keep private-tracker passkeys from leaking.
	Private        bool   `json:"private,omitempty"`
	TorrentFileURL string `json:"torrent_file_url,omitempty"`
}

// SiteClient communicates with the indexer site via HTTP.
type SiteClient struct {
	baseURL string
	token   string
	http    *http.Client
	// lastOKNano is the UnixNano timestamp of the most recent RoundTrip
	// that returned a response (any status). Populated by the transport
	// wrapper below and read by the watchdog to detect extended
	// site-unreachability (e.g. VPN tunnel dropped, DNS broken).
	lastOKNano atomic.Int64
}

// versionHeaderTransport adds X-Agent-Protocol and X-Agent-Version to every
// outbound request so the site can gate agents below its required minimum
// protocol version, and records the timestamp of any successful roundtrip
// so the watchdog can act on sustained network failure.
type versionHeaderTransport struct {
	inner  http.RoundTripper
	client *SiteClient
}

func (t *versionHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Agent-Protocol", fmt.Sprintf("%d", AgentProtocolVersion))
	req.Header.Set("X-Agent-Version", AgentVersion)
	resp, err := t.inner.RoundTrip(req)
	if err == nil && t.client != nil {
		t.client.lastOKNano.Store(time.Now().UnixNano())
	}
	return resp, err
}

// New creates a SiteClient from config.
func New(cfg *config.Config) *SiteClient {
	c := &SiteClient{
		baseURL: cfg.SiteURL,
		token:   cfg.AgentToken,
	}
	// Seed lastOK with the current time so the watchdog doesn't fire on
	// startup before any request has had a chance to complete.
	c.lastOKNano.Store(time.Now().UnixNano())
	c.http = &http.Client{
		Timeout:   120 * time.Second,
		Transport: &versionHeaderTransport{inner: http.DefaultTransport, client: c},
	}
	return c
}

// LastOK returns the timestamp of the most recent successful HTTP roundtrip
// to the site. A roundtrip is "successful" if the transport got a response
// (any status code), which is enough to prove DNS + TCP + TLS all worked.
func (c *SiteClient) LastOK() time.Time {
	return time.Unix(0, c.lastOKNano.Load())
}

// UpgradeRequiredError is returned when the site refuses the agent because
// its reported X-Agent-Protocol is below the site's minimum. The operator
// should update the binary; there is no retry loop that can recover from
// this on its own.
type UpgradeRequiredError struct {
	MinProtocol int
	Message     string
}

func (e *UpgradeRequiredError) Error() string {
	return fmt.Sprintf("agent upgrade required: site needs protocol v%d (this agent is v%d) — %s",
		e.MinProtocol, AgentProtocolVersion, e.Message)
}

// IsUpgradeRequired reports whether err is an UpgradeRequiredError.
func IsUpgradeRequired(err error) (*UpgradeRequiredError, bool) {
	var ue *UpgradeRequiredError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// parseUpgradeRequired decodes a 426 response body into an UpgradeRequiredError.
// Accepts {"min_protocol":N,"message":"..."} or falls back to the raw body.
func parseUpgradeRequired(body []byte) *UpgradeRequiredError {
	var m struct {
		MinProtocol int    `json:"min_protocol"`
		Message     string `json:"message"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &m)
	msg := m.Message
	if msg == "" {
		msg = m.Error
	}
	if msg == "" {
		msg = string(body)
	}
	return &UpgradeRequiredError{MinProtocol: m.MinProtocol, Message: msg}
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
	if resp.StatusCode == http.StatusUpgradeRequired {
		body, _ := io.ReadAll(resp.Body)
		return nil, parseUpgradeRequired(body)
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

// FetchCachedTorrentByInfoHash asks the site for a server-pre-fetched
// .torrent blob keyed by info_hash. Returns (nil, nil) on a 404 (no
// cache entry yet — the caller falls back to its own DHT lookup), or
// (nil, err) on a real failure (network, auth, parse). The site's
// metadata-prefetch worker populates these in the background; using
// the cache lets us skip the 2-minute DHT round-trip when it's there.
func (c *SiteClient) FetchCachedTorrentByInfoHash(infoHash string) ([]byte, error) {
	url := fmt.Sprintf("%s/api/agent/cached-torrent/%s", c.baseURL, infoHash)
	req, err := http.NewRequest("GET", url, nil)
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
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cached-torrent fetch returned %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}

// FetchTorrentFile downloads a .torrent file from the site. urlPath is the
// path returned in AgentTask.TorrentFileURL (e.g. "/api/agent/torrent/42").
// Absolute URLs are also accepted, in case the site ever starts returning
// them. The Authorization header is attached so the site can re-verify the
// caller owns the referenced lock.
func (c *SiteClient) FetchTorrentFile(urlPath string) ([]byte, error) {
	fullURL := urlPath
	if !strings.HasPrefix(fullURL, "http://") && !strings.HasPrefix(fullURL, "https://") {
		fullURL = c.baseURL + fullURL
	}
	req, err := http.NewRequest("GET", fullURL, nil)
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
		return nil, fmt.Errorf("torrent file not found (site returned 404 — lock may have expired)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("torrent fetch returned %d: %s", resp.StatusCode, body)
	}
	// Bound the read so a malicious or corrupted site response can't eat
	// all our memory. 10 MB matches the server-side upload cap.
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
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

// PutWebConfig writes (or clears) a single web-tier override on the site,
// as this agent. Empty value means "clear the override" — matches the
// wire contract of PUT /api/agent/web-config on the site side.
//
// Used by the local UI's "Agent settings" form to let the operator
// manage site-side config without logging into the site. The agent's
// next poll picks up the new WebOverrides and re-applies them through
// config.Layered.ApplyWeb so the change takes effect without a
// restart.
func (c *SiteClient) PutWebConfig(key, value string) error {
	body, _ := json.Marshal(map[string]string{"key": key, "value": value})
	req, err := http.NewRequest("PUT", c.baseURL+"/api/agent/web-config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("web-config write returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// SiteAgentGroup mirrors the site's AgentGroup model. Decoded verbatim
// from GET /api/agent/groups; the caller converts it into the local
// storage.Group shape (source='site') before upserting into SQLite.
// Field types match the site: *int / *bool for nullable overrides,
// []string for the array columns.
type SiteAgentGroup struct {
	ID               int      `json:"id"`
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	Newsgroups       []string `json:"newsgroups"`
	BannedExtensions []string `json:"banned_extensions"`
	Screenshots      *int     `json:"screenshots,omitempty"`
	SampleSeconds    *int     `json:"sample_seconds,omitempty"`
	Par2Redundancy   *int     `json:"par2_redundancy,omitempty"`
	Obfuscate        *bool    `json:"obfuscate,omitempty"`
	WatermarkText    string   `json:"watermark_text"`
	Version          int      `json:"version"`
}

// AgentGroupsResponse is the wire shape of GET /api/agent/groups:
// {max_version: N, groups: [...]}. The agent uses max_version as the
// since_version query param on its next poll so a steady-state fetch
// (no changes) is a single-int comparison server-side.
type AgentGroupsResponse struct {
	MaxVersion int              `json:"max_version"`
	Groups     []SiteAgentGroup `json:"groups"`
}

// FetchAgentGroups pulls the site-managed catalog of posting groups.
// sinceVersion should be the agent's last-seen max_version from the
// previous poll (0 on first boot).
//
// A 404 means the site hasn't shipped this endpoint yet — treat as
// empty rather than an error so old sites don't break new agents.
func (c *SiteClient) FetchAgentGroups(sinceVersion int) (*AgentGroupsResponse, error) {
	url := fmt.Sprintf("%s/api/agent/groups?since_version=%d", c.baseURL, sinceVersion)
	req, err := http.NewRequest("GET", url, nil)
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
		// Site hasn't been upgraded — treat as "no groups yet".
		return &AgentGroupsResponse{Groups: []SiteAgentGroup{}}, nil
	}
	// 503 with a maintenance body is the site telling all agents to back
	// off during a vacuum / migration. Surface it as a typed error so the
	// caller can downgrade it from "error" to "info" in logs (sync is
	// optional and we're going to retry on the next tick anyway).
	if resp.StatusCode == http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		var m MaintenanceResponse
		if json.Unmarshal(body, &m) == nil && m.Maintenance {
			return nil, &MaintenanceError{Info: m}
		}
		return nil, fmt.Errorf("agent groups returned 503: %s", body)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent groups returned %d", resp.StatusCode)
	}
	var out AgentGroupsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
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
func (c *SiteClient) ReportProgress(lockID int, progress, speed string, warnings []LockWarning) error {
	// warnings is serialized as its own JSON-encoded string field so the
	// site can store it verbatim in the JSONB column without re-marshal.
	warnJSON := "[]"
	if len(warnings) > 0 {
		if b, err := json.Marshal(warnings); err == nil {
			warnJSON = string(b)
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"lock_id":  lockID,
		"progress": progress,
		"speed":    speed,
		"warnings": warnJSON,
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
	Name        string         `json:"name"`
	Size        int64          `json:"size"`
	Transferred int64          `json:"transferred"`
	Percent     float64        `json:"percent"`
	Speed       string         `json:"speed,omitempty"`
	UpSpeed     string         `json:"up_speed,omitempty"`
	Phase       string         `json:"phase"`
	Peers       int            `json:"peers,omitempty"`
	Warnings    []LockWarning  `json:"warnings,omitempty"`
}

// LockWarning mirrors the site's models.LockWarning. The agent emits
// one of these per currently-counting skip rule (slow speed, low
// peers, stalled) so the dashboard can surface an icon with a live
// countdown before the rule fires.
type LockWarning struct {
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	Icon      string    `json:"icon"`
	ExpiresAt time.Time `json:"expires_at"`
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
	FailReason  string // human-readable reason when Status == "failed"
	NzbData     []byte
	Password    string
	MediaInfo   interface{} // *services.VideoInfo — JSON-serialized
	Screenshots []string    // file paths to screenshot JPEGs
	// TotalSizeBytes is the torrent's discovered TotalLength from
	// metainfo. Reported on successful completion AND on oversize-abort so
	// the site can record it on nzb_requests.size_bytes — future poll
	// queries skip the same torrent for any agent whose free disk is
	// smaller. Zero means "unknown" and is omitted from the form.
	TotalSizeBytes int64
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
	if result.FailReason != "" {
		w.WriteField("fail_reason", result.FailReason)
	}
	if result.Password != "" {
		w.WriteField("password", result.Password)
	}
	if result.TotalSizeBytes > 0 {
		w.WriteField("total_size_bytes", fmt.Sprintf("%d", result.TotalSizeBytes))
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

// Backfill re-submits an NZB from a local backup file (e.g. one written by
// the agent when the original Complete call to the site failed). The site
// performs the same hash/dedup/insert/fulfill flow as Complete but doesn't
// require a lock_id. Returns the resulting nzb_id on success.
func (c *SiteClient) Backfill(requestID int64, nzbData []byte, password string) (int64, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("request_id", fmt.Sprintf("%d", requestID))
	if password != "" {
		w.WriteField("password", password)
	}
	if part, err := w.CreateFormFile("nzb_data", "release.nzb"); err == nil {
		part.Write(nzbData)
	}
	ct := w.FormDataContentType()
	w.Close()

	resp, err := c.postGzipped(c.baseURL+"/api/agent/backfill", buf.Bytes(), ct)
	if err != nil {
		return 0, err
	}
	if v, ok := resp["nzb_id"]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n), nil
		case json.Number:
			id, _ := n.Int64()
			return id, nil
		}
	}
	return 0, nil
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
