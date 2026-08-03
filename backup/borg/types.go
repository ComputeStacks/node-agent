package borg

import (
	"cs-agent/containermgr"
	"cs-agent/store"

	"github.com/ghodss/yaml"
)

/**
Archive Structures
*/

// Used for created and managing archives
type Archive struct {
	Name       string
	Repository *Repository

	// Warning is borg's own record when create completed with a benign warning — it
	// wrote the archive, and something (a file changed mid-read) was logged at WARNING.
	// Set by Create, which clears it first; nil on a clean run and on a failure. Not an
	// error: the task succeeds and last_backup advances.
	Warning *LogMessage
}

// Response structures from borg.
type ArchiveMessage struct {
	Archive struct {
		ID       string      `json:"id"`
		Duration float64     `json:"duration"`
		Start    BTimeFormat `json:"start"`
		End      BTimeFormat `json:"end"`
		Stats    struct {
			CompressedSize int `json:"compressed_size"`
			DedupedSize    int `json:"deduplicated_size"`
			FileCount      int `json:"nfiles"`
			OriginalSize   int `json:"original_size"`
		} `json:"stats"`
	} `json:"archive"`
	Encryption EncryptionItem `json:"encryption"`
	Repository RepositoryItem `json:"repository"`
}

type ArchiveResponse struct {
	ArchiveItems []struct {
		ID          string      `json:"id"`
		Name        string      `json:"name"`
		Username    string      `json:"username"`
		CommandLine []string    `json:"command_line"`
		Comment     string      `json:"comment"`
		Duration    float64     `json:"duration"`
		Start       BTimeFormat `json:"start"`
		End         BTimeFormat `json:"end"`
		Hostname    string      `json:"hostname"`
		Limits      struct {
			MaxArchiveSize float64 `json:"max_archive_size"`
		} `json:"limits"`
	} `json:"archives"`
	Cache      CacheItem      `json:"cache"`
	Encryption EncryptionItem `json:"encryption"`
	Repository RepositoryItem `json:"repository"`
}

// Error log format for borg
type LogMessage struct {
	Time      float64 `json:"time"`
	Type      string  `json:"type"`
	Message   string  `json:"message"`
	MsgID     string  `json:"msgid"`
	LevelName string  `json:"levelname"`
	Name      string  `json:"name"`
}

// Repo format from ComputeStack Volumes
type Repository struct {
	Name             string
	SourceVolumeName string
	Container        *containermgr.Container // Track what container we're using to perform this backup
	// Store is the control.db handle used to report observed repo state UP
	// (size/archives) via Sync — the successor to the Consul borg/repository key.
	// Set at construction (FindRepository / the &Repository{} literals); Sync is a
	// no-op when nil.
	Store *store.Store
	//ContainerConfig        *BorgContainerConfig
	Strategy               string   `json:"strategy"`
	PreBackup              []string `json:"pre_backup"`
	PostBackup             []string `json:"post_backup"`
	PreRestore             []string `json:"pre_restore"`
	PostRestore            []string `json:"post_restore"`
	BackupContinueOnError  bool     `json:"backup_error_cont"`  // Continue if an error is encountered with `pre_backup`
	RestoreContinueOnError bool     `json:"restore_error_cont"` // Continue if an error is encountered with `pre_restore`
	Retention              struct {
		Hourly   int `json:"keep_hourly"`
		Daily    int `json:"keep_daily"`
		Weekly   int `json:"keep_weekly"`
		Monthly  int `json:"keep_monthly"`
		Annually int `json:"keep_annually"`
	} `json:"retention"`
}

// Returned by `Repository.Contents()`
type RepositoryContentResponse struct {
	Archives []struct {
		Archive  string      `json:"archive"`
		BArchive string      `json:"barchive"`
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Start    BTimeFormat `json:"start"`
		Time     BTimeFormat `json:"time"`
	} `json:"archives"`
	Encryption EncryptionItem `json:"encryption"`
	Repository RepositoryItem `json:"repository"`
}

// Returned by `Repository.Info()`
type RepositoryResponse struct {
	Cache       CacheItem      `json:"cache"`
	Encryption  EncryptionItem `json:"encryption"`
	Repository  RepositoryItem `json:"repository"`
	SecurityDir string         `json:"security_dir"`
}

// Common structures used in various other structs
type RepositoryItem struct {
	ID           string      `json:"id"`
	LastModified BTimeFormat `json:"last_modified"`
	Location     string      `json:"location"`
}
type EncryptionItem struct {
	Mode string `json:"mode"`
}
type CacheItem struct {
	Path  string `json:"path"`
	Stats struct {
		TotalChunks       int `json:"total_chunks"`
		TotalCSize        int `json:"total_csize"`
		TotalSize         int `json:"total_size"`
		TotalUniqueChunks int `json:"total_unique_chunks"`
		UniqueCSize       int `json:"unique_csize"`
		UniqueSize        int `json:"unique_size"`
	} `json:"stats"`
}

// Formatting to YAML
func (b *ArchiveMessage) ToYaml() string {
	if y, err := yaml.Marshal(b); err == nil {
		return string(y)
	}
	return ""
}

func (b *ArchiveResponse) ToYaml() string {
	if y, err := yaml.Marshal(b); err == nil {
		return string(y)
	}
	return ""
}

func (b *LogMessage) ToYaml() string {
	if y, err := yaml.Marshal(b); err == nil {
		return string(y)
	}
	return ""
}

func (b *RepositoryContentResponse) ToYaml() string {
	if y, err := yaml.Marshal(b); err == nil {
		return string(y)
	}
	return ""
}

func (b *RepositoryResponse) ToYaml() string {
	if y, err := yaml.Marshal(b); err == nil {
		return string(y)
	}
	return ""
}
