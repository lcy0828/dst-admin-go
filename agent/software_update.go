package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"dont/internal/softwareupdate"
	"dont/shared"
)

func (a *Agent) ConfigureSoftwareUpdate(service *softwareupdate.Service) {
	a.software = service
	service.SetRestartGuard(func(context.Context) (func(), error) {
		a.commandAdmissionMu.Lock()
		defer a.commandAdmissionMu.Unlock()
		// This command is included in commandsPending. No other command may be
		// queued or running when the software switches; DST processes may run.
		if a.commandsPaused || a.commandsPending != 1 {
			return nil, softwareupdate.ErrBusy
		}
		a.commandsPaused = true
		return func() { a.commandAdmissionMu.Lock(); a.commandsPaused = false; a.commandAdmissionMu.Unlock() }, nil
	})
}

func (a *Agent) executeSoftwareUpdate(request *shared.AgentUpgradeRequest) (shared.AgentUpgradeResult, error) {
	result := shared.AgentUpgradeResult{ProtocolVersion: shared.AgentUpgradeProtocolVersion, ReleaseID: request.ReleaseID, PreviousVersion: AgentVersion, ObservedAt: time.Now().UTC()}
	if a.software == nil || request.ProtocolVersion != shared.AgentUpgradeProtocolVersion {
		return result, softwareupdate.ErrUnsupported
	}
	if err := shared.ValidateAgentSoftwareRequest(*request); err != nil {
		return result, err
	}
	if request.OS != runtime.GOOS || request.Arch != runtime.GOARCH {
		return result, softwareupdate.ErrInvalid
	}
	source := request.Source
	if source == "" {
		source = "auto"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var snapshot softwareupdate.Snapshot
	var err error
	switch request.Action {
	case "status":
		snapshot, err = a.software.Snapshot()
	case "check":
		snapshot, err = a.software.Check(ctx, request.Force, source)
	case "update":
		if source == "controller" {
			var client *controllerUpdateClient
			client, err = newControllerUpdateClient(a.Config.ServerURL, *request)
			if err == nil {
				_, err = a.software.StartWithClient(request.Version, "auto", client)
			}
		} else {
			_, err = a.software.Start(request.Version, source)
		}
		if err == nil {
			snapshot, err = a.software.Snapshot()
		}
	case "apply":
		snapshot, err = a.software.Snapshot()
		if err == nil && (snapshot.Operation == nil || snapshot.Operation.ID != request.OperationID || snapshot.Operation.Version != request.Version) {
			err = softwareupdate.ErrInvalid
		}
		if err == nil {
			var resume func()
			_, resume, err = a.software.PrepareApply(ctx, request.OperationID)
			if err == nil {
				snapshot, err = a.software.Snapshot()
				a.software.NotifyApply(resume)
			}
		}
	default:
		err = softwareupdate.ErrInvalid
	}
	if err == nil {
		result.Software, err = json.Marshal(snapshot)
		result.Version = snapshot.Current.Version
	}
	return result, err
}

type controllerUpdateClient struct {
	release         softwareupdate.Release
	url, token, sha string
}

func newControllerUpdateClient(serverURL string, request shared.AgentUpgradeRequest) (*controllerUpdateClient, error) {
	// A relay URL is always resolved against the already authenticated control
	// connection. Neither the browser nor release metadata can choose a host.
	if request.DownloadPath != "/agent-software-updates/"+request.ReleaseID || request.Size < 1 || request.Size > softwareupdate.MaxArchiveBytes || len(request.SHA256) != 64 || len(request.DownloadToken) < 16 || len(request.DownloadToken) > 256 {
		return nil, softwareupdate.ErrInvalid
	}
	for _, c := range request.SHA256 {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, softwareupdate.ErrInvalid
		}
	}
	address, err := resolveControllerDownloadURL(serverURL, request.DownloadPath, "/agent-software-updates/")
	if err != nil {
		return nil, err
	}
	name := "dst-admin-agent-update-" + request.OS + "-" + request.Arch + ".tar.gz"
	return &controllerUpdateClient{url: address, token: request.DownloadToken, sha: request.SHA256, release: softwareupdate.Release{Version: request.Version, OnlineUpdate: true, Archive: softwareupdate.Asset{Name: name, Size: request.Size, Digest: "sha256:" + request.SHA256}, Checksum: softwareupdate.Asset{Name: name + ".sha256", Size: 100}}}, nil
}
func (c *controllerUpdateClient) Latest(context.Context, string, string) (*softwareupdate.Release, error) {
	return &c.release, nil
}
func (c *controllerUpdateClient) Checksum(context.Context, softwareupdate.Asset, string) ([]byte, error) {
	return []byte(c.sha + "  " + c.release.Archive.Name + "\n"), nil
}
func (c *controllerUpdateClient) Download(ctx context.Context, _ softwareupdate.Asset, _ string, writer io.Writer) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	client := &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return softwareupdate.ErrInvalid }}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("controller transfer: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > 0 && response.ContentLength != c.release.Archive.Size {
		return softwareupdate.ErrInvalid
	}
	n, err := io.Copy(writer, io.LimitReader(response.Body, c.release.Archive.Size+1))
	if err != nil {
		return err
	}
	if n != c.release.Archive.Size {
		return softwareupdate.ErrInvalid
	}
	return nil
}
