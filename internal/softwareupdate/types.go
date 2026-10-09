// Package softwareupdate manages official management-service releases. Game
// installations, saves, configuration and standalone Agents are separate data.
package softwareupdate

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dont/internal/buildinfo"
)

const (
	Repository              = "lcy0828/dst-admin-go"
	Protocol                = 1
	MaxArchiveBytes   int64 = 200 << 20
	MaxExtractedBytes int64 = 512 << 20
)

var (
	ErrBusy           = errors.New("software update is busy")
	ErrUnsupported    = errors.New("online software updates are unavailable for this deployment")
	ErrInvalid        = errors.New("invalid software update request or package")
	ErrReleaseChanged = errors.New("the selected release is no longer the latest compatible release")
	ErrNotPrepared    = errors.New("no verified software update is prepared")
	versionPattern    = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

func NormalizeVersion(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if !versionPattern.MatchString(value) {
		return "", false
	}
	for _, number := range strings.Split(strings.TrimPrefix(value, "v"), ".") {
		if _, err := strconv.ParseUint(number, 10, 32); err != nil {
			return "", false
		}
	}
	return "v" + strings.TrimPrefix(value, "v"), true
}

func CompareVersions(left, right string) int {
	l, lok := NormalizeVersion(left)
	r, rok := NormalizeVersion(right)
	if !lok || !rok {
		return 0
	}
	la, ra := strings.Split(l[1:], "."), strings.Split(r[1:], ".")
	for i := range la {
		x, _ := strconv.ParseUint(la[i], 10, 32)
		y, _ := strconv.ParseUint(ra[i], 10, 32)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

type Release struct {
	Version      string    `json:"version"`
	Name         string    `json:"name"`
	Notes        string    `json:"notes"`
	URL          string    `json:"url"`
	PublishedAt  time.Time `json:"publishedAt"`
	Archive      Asset     `json:"-"`
	Checksum     Asset     `json:"-"`
	OnlineUpdate bool      `json:"onlineUpdate"`
}

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type Check struct {
	Latest    *Release  `json:"latest,omitempty"`
	HasUpdate bool      `json:"hasUpdate"`
	CheckedAt time.Time `json:"checkedAt"`
	Cached    bool      `json:"cached"`
	Warning   string    `json:"warning,omitempty"`
}

type Operation struct {
	ID              string    `json:"id"`
	Version         string    `json:"version"`
	Phase           string    `json:"phase"`
	Progress        int       `json:"progress"`
	DownloadedBytes int64     `json:"downloadedBytes"`
	TotalBytes      int64     `json:"totalBytes"`
	BytesPerSecond  int64     `json:"bytesPerSecond"`
	StartedAt       time.Time `json:"startedAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	Error           string    `json:"error,omitempty"`
	ReleaseID       string    `json:"releaseId,omitempty"`
}

func (o Operation) Busy() bool {
	return o.Phase == "downloading" || o.Phase == "verifying" || o.Phase == "restarting"
}

type Snapshot struct {
	Current           buildinfo.Info `json:"current"`
	Platform          string         `json:"platform"`
	Supported         bool           `json:"supported"`
	Ready             bool           `json:"ready"`
	UnsupportedReason string         `json:"unsupportedReason,omitempty"`
	Check             Check          `json:"check"`
	Operation         *Operation     `json:"operation,omitempty"`
}

type Manifest struct {
	Kind           string                `json:"kind,omitempty"`
	Protocol       int                   `json:"protocol"`
	Version        string                `json:"version"`
	Platform       string                `json:"platform"`
	EmbeddedWebUI  bool                  `json:"embeddedWebUI"`
	FrontendCommit string                `json:"frontendCommit"`
	Files          map[string]FileDigest `json:"files"`
}

type FileDigest struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type installedRelease struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type diskState struct {
	Protocol    int               `json:"protocol"`
	BaseVersion string            `json:"baseVersion,omitempty"`
	Current     *installedRelease `json:"current,omitempty"`
	Previous    *installedRelease `json:"previous,omitempty"`
	Pending     *installedRelease `json:"pending,omitempty"`
	Operation   *Operation        `json:"operation,omitempty"`
}
