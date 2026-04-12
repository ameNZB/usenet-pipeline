package storage

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type JobState struct {
	Name           string  `json:"name"`
	Phase          string  `json:"phase"`
	Details        string  `json:"details"`
	Progress       float64 `json:"progress"`
	UpdatedAt      string  `json:"updated_at"`
	DownloadedPath string  `json:"downloaded_path,omitempty"`
	Password       string  `json:"password,omitempty"`
	ZipName        string  `json:"zip_name,omitempty"`
}

type AppState struct {
	sync.RWMutex
	PublicIP    string               `json:"public_ip"`
	VPNStatus   string               `json:"vpn_status"`
	VPNProvider string               `json:"vpn_provider"`
	WatchFiles  []string             `json:"watch_files"`
	Jobs        map[string]*JobState `json:"jobs"`
	NNTPDomain  string               `json:"nntp_domain"`
	NNTPPoster  string               `json:"nntp_poster"`
	QueuePaused bool                 `json:"queue_paused"`
}

var GlobalState = AppState{Jobs: make(map[string]*JobState)}
var JobCancels sync.Map

var StateFile string

func SaveState() {
	if StateFile == "" {
		return
	}
	GlobalState.RLock()
	data, err := json.MarshalIndent(GlobalState.Jobs, "", "  ")
	GlobalState.RUnlock()
	if err == nil {
		os.WriteFile(StateFile, data, 0644)
	}
}

func LoadState() {
	if StateFile == "" {
		return
	}
	data, err := os.ReadFile(StateFile)
	if err == nil {
		var jobs map[string]*JobState
		if err := json.Unmarshal(data, &jobs); err == nil {
			GlobalState.Lock()
			GlobalState.Jobs = jobs
			GlobalState.Unlock()
		}
	}
}

func UpdateJobMeta(name, path, password, zipName string) {
	GlobalState.Lock()
	if job, exists := GlobalState.Jobs[name]; exists {
		if path != "" {
			job.DownloadedPath = path
		}
		if password != "" {
			job.Password = password
		}
		if zipName != "" {
			job.ZipName = zipName
		}
	}
	GlobalState.Unlock()
	SaveState()
}

// UpdateState safely modifies the tracked status of a pipeline job
func UpdateState(name, phase, details string, progress float64) {
	GlobalState.Lock()

	if _, exists := GlobalState.Jobs[name]; !exists {
		GlobalState.Jobs[name] = &JobState{Name: name}
	}

	GlobalState.Jobs[name].Phase = phase
	GlobalState.Jobs[name].Details = details
	GlobalState.Jobs[name].Progress = progress
	GlobalState.Jobs[name].UpdatedAt = time.Now().Format("15:04:05")
	GlobalState.Unlock()
	SaveState()
}
