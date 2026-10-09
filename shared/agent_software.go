package shared

import (
	"fmt"
	"regexp"
	"strings"
)

var agentSoftwareID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var agentSoftwareVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var agentSoftwareDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// This typed command never accepts executable paths or arbitrary download URLs.
func ValidateAgentSoftwareRequest(request AgentUpgradeRequest) error {
	invalid := fmt.Errorf("Agent 软件更新请求无效")
	if request.ProtocolVersion != AgentUpgradeProtocolVersion || !agentSoftwareID.MatchString(request.ReleaseID) || (request.OS != "linux" && request.OS != "darwin" && request.OS != "windows") || (request.Arch != "amd64" && request.Arch != "arm64") {
		return invalid
	}
	if request.Source != "" && request.Source != "auto" && request.Source != "direct" && request.Source != "proxy" && request.Source != "controller" {
		return invalid
	}
	switch request.Action {
	case "status", "check":
		if request.Version != "" || request.OperationID != "" || request.Source == "controller" {
			return invalid
		}
	case "update":
		if !agentSoftwareVersion.MatchString(request.Version) || len(request.Version) > 64 {
			return invalid
		}
	case "apply":
		if !agentSoftwareID.MatchString(request.OperationID) || !agentSoftwareVersion.MatchString(request.Version) || len(request.Version) > 64 {
			return invalid
		}
	default:
		return invalid
	}
	if request.Source == "controller" && request.Action == "update" {
		if request.DownloadPath != "/agent-software-updates/"+request.ReleaseID || len(request.DownloadToken) < 16 || len(request.DownloadToken) > 256 || strings.ContainsAny(request.DownloadToken, "\x00\r\n") || !agentSoftwareDigest.MatchString(request.SHA256) || request.Size < 1 || request.Size > 200<<20 {
			return invalid
		}
	} else if request.DownloadPath != "" || request.DownloadToken != "" || request.SHA256 != "" || request.Size != 0 {
		return invalid
	}
	return nil
}
