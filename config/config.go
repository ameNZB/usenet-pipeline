package config

import (
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	// Site connection
	SiteURL      string
	AgentToken   string
	PollInterval int // seconds between polls

	// Directories
	TempDir  string
	WatchDir string // legacy — kept for local .torrent fallback

	// VPN
	VPNProvider     string
	VPNDownloadOnly bool   // route only torrent traffic through VPN (SOCKS5); uploads go direct
	VPNProxyAddr    string // SOCKS5 proxy address (e.g. "vpn:1080") when VPNDownloadOnly=true

	// NNTP
	NNTPServer      string
	NNTPSSL         bool
	NNTPConnections int
	NNTPUser        string
	NNTPPass        string
	NNTPGroup       string
	NNTPPoster      string
	NNTPFrom        string
	NNTPDomain      string

	// PAR2
	PAR2Redundancy int // recovery percentage (default 5)
	PAR2Threads    int // 0 = all cores (parpar default), >0 = limit threads
	PAR2Memory     int // MB; 0 = auto (parpar default), >0 = cap memory usage

	// Concurrency
	MaxConcurrentDownloads int // how many torrents to download in parallel (default 3)

	// Disk
	MaxDiskUsageGB float64 // max temp disk usage in GB (0 = no limit, uses all available)

	// CPU throttle
	CPUMaxPercent float64 // don't start new tasks above this CPU usage (default 85, 0 = disabled)

	// Slow download rejection
	SlowSpeedThresholdMBs float64 // MB/s below which download is "slow" (default 0.05)
	SlowSpeedTimeoutMins  int     // minutes of sustained slow speed before rejecting (default 10)

	// Branding
	GeneratorName string // NZB x-generator header (default "usenet-pipeline")

	// Obfuscation & Encryption
	Obfuscate bool // rename files to random hex before upload (default false)
	Encrypt   bool // wrap files in password-protected 7z before upload

	// Layered holds the yml/env/web tiers for settings that are tunable via
	// the site or local web UI. The fields above continue to be populated
	// from the *effective* tier at construction time so legacy readers see
	// the merged value, but runtime changes arriving from the site are
	// applied through Layered.ApplyWeb and then mirrored back via Refresh.
	Layered *Layered `json:"-"`
}

// ConfigYmlPath returns the path to the layered YAML file, honouring
// CONFIG_YML when set and otherwise sitting beside TempDir's parent.
func ConfigYmlPath() string {
	if p := os.Getenv("CONFIG_YML"); p != "" {
		return p
	}
	base := getEnv("CONFIG_DIR", ".")
	return filepath.Join(base, "config.yml")
}

func NewConfig() *Config {
	l := NewLayered(ConfigYmlPath())
	return newConfigFromLayered(l)
}

func newConfigFromLayered(l *Layered) *Config {
	layeredInt := func(key string, fallback int) int {
		if v := l.Effective(key); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				return i
			}
		}
		return fallback
	}
	layeredFloat := func(key string, fallback float64) float64 {
		if v := l.Effective(key); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
		return fallback
	}
	return &Config{
		Layered: l,
		SiteURL:                getEnv("SITE_URL", ""),
		AgentToken:             getEnv("AGENT_TOKEN", ""),
		PollInterval:           getEnvAsInt("POLL_INTERVAL", 30),
		TempDir:                getEnv("TEMP_DIR", "./temp"),
		WatchDir:               getEnv("WATCH_DIR", "./watch"),
		VPNProvider:            getEnv("VPN_PROVIDER", "Unknown"),
		VPNDownloadOnly:        getEnv("VPN_DOWNLOAD_ONLY", "false") == "true",
		VPNProxyAddr:           getEnv("VPN_PROXY_ADDR", "vpn:1080"),
		NNTPServer:             getEnv("NNTP_SERVER", "news.example.com:119"),
		NNTPSSL:                getEnv("NNTP_SSL", "false") == "true",
		NNTPConnections:        getEnvAsInt("NNTP_CONNECTIONS", 10),
		NNTPUser:               getEnv("NNTP_USER", "username"),
		NNTPPass:               getEnv("NNTP_PASS", "password"),
		NNTPGroup:              getEnv("NNTP_GROUP", "alt.binaries.test"),
		NNTPPoster:             getEnv("NNTP_POSTER", "Pipeline_Uploader"),
		NNTPFrom:               getEnv("NNTP_FROM", "uploader@yourdomain.com"),
		NNTPDomain:             getEnv("NNTP_DOMAIN", ""),
		PAR2Redundancy:         getEnvAsInt("PAR2_REDUNDANCY", 5),
		PAR2Threads:            getEnvAsInt("PAR2_THREADS", 0),
		PAR2Memory:             getEnvAsInt("PAR2_MEMORY_MB", 0),
		MaxDiskUsageGB:         layeredFloat("max_disk_usage_gb", 0),
		MaxConcurrentDownloads: layeredInt("max_concurrent_downloads", 3),
		CPUMaxPercent:          layeredFloat("cpu_max_percent", 85),
		SlowSpeedThresholdMBs:  layeredFloat("slow_speed_threshold_mbs", 0.05),
		SlowSpeedTimeoutMins:   layeredInt("slow_speed_timeout_mins", 10),
		GeneratorName:          getEnv("GENERATOR_NAME", "usenet-pipeline"),
		Obfuscate:              getEnv("OBFUSCATE", "false") == "true",
		Encrypt:                getEnv("ENCRYPT", "false") == "true",
	}
}

// Refresh re-derives the layered fields on Config after the web tier has
// been replaced via Layered.ApplyWeb. Env-scoped fields are untouched.
func (c *Config) Refresh() {
	if c.Layered == nil {
		return
	}
	fresh := newConfigFromLayered(c.Layered)
	c.MaxDiskUsageGB = fresh.MaxDiskUsageGB
	c.MaxConcurrentDownloads = fresh.MaxConcurrentDownloads
	c.CPUMaxPercent = fresh.CPUMaxPercent
	c.SlowSpeedThresholdMBs = fresh.SlowSpeedThresholdMBs
	c.SlowSpeedTimeoutMins = fresh.SlowSpeedTimeoutMins
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvAsFloat(key string, fallback float64) float64 {
	if value, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return fallback
}
