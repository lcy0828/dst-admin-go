package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dont/internal/shardtransfer"
	"dont/shared"
)

const migrationPeerGrantTTL = 35 * time.Minute

type migrationPeerGrant struct {
	InstallationID string
	MigrationID    string
	PeerSubject    string
	Size           int64
	SHA256         string
	ExpiresAt      time.Time
}

func (a *Agent) observeMigrationPeerGrant(installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "迁移制品 Peer 授权已签发")
	location, err := a.issueMigrationPeerGrant(installation, *request.Migration)
	result.Migration = &shared.RuntimeMigrationResult{MigrationID: request.Migration.MigrationID}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	result.Migration.Size, result.Migration.SHA256 = location.Size, location.SHA256
	result.Migration.FetchLocation, result.Migration.Complete = &location, true
	return result, nil
}

func (a *Agent) issueMigrationPeerGrant(installation RuntimeInstallation, request shared.RuntimeMigrationRequest) (shared.RuntimeMigrationFetchLocation, error) {
	if !a.modPeerEnabled() {
		return shared.RuntimeMigrationFetchLocation{}, errors.New("Agent 未启用 Runtime Peer 服务")
	}
	manager, err := a.transferManager(installation)
	if err != nil {
		return shared.RuntimeMigrationFetchLocation{}, err
	}
	descriptor, file, err := manager.OpenExport(request.MigrationID)
	if err != nil {
		return shared.RuntimeMigrationFetchLocation{}, err
	}
	_ = file.Close()
	if descriptor.Size != request.Size || !strings.EqualFold(descriptor.SHA256, request.SHA256) {
		return shared.RuntimeMigrationFetchLocation{}, shardtransfer.ErrIntegrity
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return shared.RuntimeMigrationFetchLocation{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expiresAt := time.Now().UTC().Add(migrationPeerGrantTTL)
	grant := migrationPeerGrant{
		InstallationID: installation.ID, MigrationID: request.MigrationID, PeerSubject: request.PeerSubject,
		Size: descriptor.Size, SHA256: strings.ToLower(descriptor.SHA256), ExpiresAt: expiresAt,
	}
	a.modPeerMu.Lock()
	a.expireMigrationPeerGrantsLocked(time.Now().UTC())
	if a.migrationPeerGrants == nil {
		a.migrationPeerGrants = make(map[string]migrationPeerGrant)
	}
	a.migrationPeerGrants[migrationPeerTokenKey(token)] = grant
	a.modPeerMu.Unlock()
	path := fmt.Sprintf("/migration-peer/%s/%s", url.PathEscape(installation.ID), url.PathEscape(request.MigrationID))
	return shared.RuntimeMigrationFetchLocation{
		DownloadURL: a.Config.ModPeerAdvertiseURL + path, DownloadPath: path, DownloadToken: token,
		Size: descriptor.Size, SHA256: strings.ToLower(descriptor.SHA256), ExpiresAt: expiresAt,
	}, nil
}

func (a *Agent) serveMigrationPeer(response http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/migration-peer/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(response, request)
		return
	}
	authorization := strings.Fields(strings.TrimSpace(request.Header.Get("Authorization")))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	subject := strings.TrimSpace(request.Header.Get("X-DST-Peer-Subject"))
	grant, ok := a.authorizeMigrationPeerGrant(parts[0], parts[1], subject, authorization[1])
	if !ok {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	installation, exists := a.runtimeInstallation(grant.InstallationID)
	if !exists {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	manager, err := a.transferManager(installation)
	if err != nil {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	descriptor, file, err := manager.OpenExport(grant.MigrationID)
	if err != nil || descriptor.Size != grant.Size || !strings.EqualFold(descriptor.SHA256, grant.SHA256) {
		if file != nil {
			_ = file.Close()
		}
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	defer file.Close()
	response.Header().Set("Cache-Control", "private, no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-DST-Migration-SHA256", descriptor.SHA256)
	response.Header().Set("X-DST-Migration-Size", fmt.Sprintf("%d", descriptor.Size))
	response.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": descriptor.MigrationID + ".zip",
	}))
	http.ServeContent(response, request, "", time.Time{}, file)
}

func (a *Agent) authorizeMigrationPeerGrant(installationID, migrationID, subject, token string) (migrationPeerGrant, bool) {
	if token == "" || subject == "" {
		return migrationPeerGrant{}, false
	}
	now := time.Now().UTC()
	a.modPeerMu.Lock()
	defer a.modPeerMu.Unlock()
	a.expireMigrationPeerGrantsLocked(now)
	grant, ok := a.migrationPeerGrants[migrationPeerTokenKey(token)]
	if !ok || grant.InstallationID != installationID || grant.MigrationID != migrationID || grant.PeerSubject != subject || !now.Before(grant.ExpiresAt) {
		return migrationPeerGrant{}, false
	}
	return grant, true
}

func (a *Agent) expireMigrationPeerGrantsLocked(now time.Time) {
	for key, grant := range a.migrationPeerGrants {
		if !now.Before(grant.ExpiresAt) {
			delete(a.migrationPeerGrants, key)
		}
	}
}

func (a *Agent) revokeMigrationPeerGrants(installationID, migrationID string) {
	a.modPeerMu.Lock()
	defer a.modPeerMu.Unlock()
	for key, grant := range a.migrationPeerGrants {
		if grant.InstallationID == installationID && grant.MigrationID == migrationID {
			delete(a.migrationPeerGrants, key)
		}
	}
}

func migrationPeerTokenKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (a *Agent) fetchMigrationImport(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeMigrationRequest) (int64, error) {
	manager, err := a.transferManager(installation)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, location := range request.FetchLocations {
		for attempt := 0; attempt < 3; attempt++ {
			_, offset, progressErr := manager.ImportProgress(request.MigrationID)
			if progressErr != nil {
				return 0, progressErr
			}
			if offset == request.Size {
				verified, verifyErr := manager.VerifyImport(ctx, request.MigrationID)
				if verifyErr == nil && verified.Size == request.Size && strings.EqualFold(verified.SHA256, request.SHA256) {
					return offset, nil
				}
				return offset, errors.Join(shardtransfer.ErrIntegrity, verifyErr)
			}
			next, fetchErr := a.fetchMigrationRange(ctx, manager, request, location, offset)
			if fetchErr == nil {
				verified, verifyErr := manager.VerifyImport(ctx, request.MigrationID)
				if verifyErr == nil && verified.Size == request.Size && strings.EqualFold(verified.SHA256, request.SHA256) {
					return next, nil
				}
				fetchErr = errors.Join(shardtransfer.ErrIntegrity, verifyErr)
			}
			failures = append(failures, fetchErr)
			if ctx.Err() != nil {
				return next, ctx.Err()
			}
		}
	}
	_, offset, _ := manager.ImportProgress(request.MigrationID)
	return offset, errors.Join(failures...)
}

func (a *Agent) fetchMigrationRange(ctx context.Context, manager *shardtransfer.Manager, migration shared.RuntimeMigrationRequest, location shared.RuntimeMigrationFetchLocation, offset int64) (int64, error) {
	downloadURL, err := resolveMigrationPeerURL(location)
	if err != nil {
		return offset, err
	}
	peerRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return offset, err
	}
	peerRequest.Header.Set("Authorization", "Bearer "+location.DownloadToken)
	peerRequest.Header.Set("X-DST-Peer-Subject", "agent:"+a.Config.AgentID)
	if offset > 0 {
		peerRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	client := &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(next *http.Request, previous []*http.Request) error {
			if len(previous) == 0 || !strings.EqualFold(next.URL.Scheme, previous[0].URL.Scheme) || !strings.EqualFold(next.URL.Host, previous[0].URL.Host) {
				return errors.New("迁移制品下载不允许跨主机重定向")
			}
			return nil
		},
	}
	response, err := client.Do(peerRequest)
	if err != nil {
		return offset, err
	}
	defer response.Body.Close()
	if offset == 0 && response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent ||
		offset > 0 && response.StatusCode != http.StatusPartialContent {
		return offset, fmt.Errorf("迁移制品下载 HTTP %d", response.StatusCode)
	}
	if response.StatusCode == http.StatusPartialContent && !validModRange(response.Header.Get("Content-Range"), offset, migration.Size) {
		return offset, errors.New("迁移制品 Content-Range 无效")
	}
	if !strings.EqualFold(response.Header.Get("X-DST-Migration-SHA256"), migration.SHA256) ||
		response.Header.Get("X-DST-Migration-Size") != fmt.Sprintf("%d", migration.Size) {
		return offset, errors.New("迁移制品响应身份无效")
	}
	remaining := migration.Size - offset
	if response.ContentLength >= 0 && response.ContentLength != remaining {
		return offset, errors.New("迁移制品响应长度与断点不一致")
	}
	return manager.ReceiveImport(ctx, migration.MigrationID, offset, io.LimitReader(response.Body, remaining))
}

func resolveMigrationPeerURL(location shared.RuntimeMigrationFetchLocation) (string, error) {
	parsed, err := url.Parse(location.DownloadURL)
	if err != nil || parsed.User != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != location.DownloadPath ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/migration-peer/") {
		return "", errors.New("迁移 Peer 下载地址无效")
	}
	return parsed.String(), nil
}

func validMigrationFetchLocation(location shared.RuntimeMigrationFetchLocation, migration shared.RuntimeMigrationRequest, now time.Time) bool {
	if location.Size != migration.Size || !strings.EqualFold(location.SHA256, migration.SHA256) ||
		!validModDownloadToken(location.DownloadToken) || !location.ExpiresAt.After(now) || location.ExpiresAt.After(now.Add(time.Hour)) {
		return false
	}
	_, err := resolveMigrationPeerURL(location)
	return err == nil
}
