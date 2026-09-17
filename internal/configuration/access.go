package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/runtimefiles"
	"dont/shared"
)

var accessIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type accessDocument struct {
	values   AccessLists
	files    map[string]fileSnapshot
	revision string
}

type fileSnapshot struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

func (s *Service) AccessLists(roomID string) (AccessLists, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.AccessListsContext(ctx, roomID)
}

func (s *Service) AccessListsContext(ctx context.Context, roomID string) (AccessLists, error) {
	_, _, document, _, err := s.accessDocument(ctx, roomID)
	if err != nil {
		return AccessLists{}, err
	}
	return document.values, nil
}

func (s *Service) PreviewAccess(roomID string, request AccessUpdateRequest) (Preview, error) {
	room, _, document, _, err := s.accessDocument(context.Background(), roomID)
	if err != nil {
		return Preview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return Preview{}, err
	}
	return prepareAccessUpdate(room.Name, document, request, false)
}

func (s *Service) ValidateAccessApply(roomID string, request AccessUpdateRequest) (Preview, error) {
	room, _, document, _, err := s.accessDocument(context.Background(), roomID)
	if err != nil {
		return Preview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return Preview{}, err
	}
	return prepareAccessUpdate(room.Name, document, request, true)
}

func (s *Service) ApplyAccess(ctx context.Context, jobID, roomID string, request AccessUpdateRequest) (ApplyResult, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer release()
	room, roomPath, document, routed, err := s.accessDocument(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return ApplyResult{}, err
	}
	preview, err := prepareAccessUpdate(room.Name, document, request, true)
	if err != nil {
		return ApplyResult{}, err
	}
	_, _, latest, latestRouted, err := s.accessDocument(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	if latestRouted != routed {
		return ApplyResult{}, &RevisionConflictError{CurrentRevision: latest.revision}
	}
	if err := checkRevision(request.ExpectedRevision, latest.revision); err != nil {
		return ApplyResult{}, err
	}
	next, _, err := normalizedAccessRequest(request)
	if err != nil {
		return ApplyResult{}, err
	}
	writes := accessWrites(latest, next)
	if routed {
		if s.publisher == nil {
			return ApplyResult{}, wrapApplyError("access list publication", errors.New("configuration publisher is unavailable"))
		}
		files := make([]rooms.ProvisionFile, 0, len(writes))
		for _, write := range writes {
			mode := latest.files[write.name].mode
			if mode == 0 {
				mode = 0o640
			}
			files = append(files, rooms.ProvisionFile{Name: write.name, Data: write.data, Mode: mode})
		}
		names := make([]string, 0, len(files))
		for _, file := range files {
			names = append(names, file.Name)
		}
		published, publishErr := s.publish(ctx, PublicationRequest{
			RoomID: roomID, Scope: PublicationShared, Files: names, Payload: files, IncludeLocal: true,
		})
		if publishErr != nil {
			return ApplyResult{}, wrapApplyError("access list publication", publishErr)
		}
		return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, PublishedTargets: published, Sync: SyncState{
			Status: "synced", Source: "runtime-disk", ObservedRevision: preview.NextRevision,
		}}, nil
	}
	if err := atomicWriteSet(roomPath, latest.files, writes); err != nil {
		return ApplyResult{}, wrapApplyError("access lists", err)
	}
	names := make([]string, 0, len(writes))
	for _, write := range writes {
		names = append(names, write.name)
	}
	files := make([]rooms.ProvisionFile, 0, len(writes))
	for _, write := range writes {
		mode := latest.files[write.name].mode
		if mode == 0 {
			mode = 0o640
		}
		files = append(files, rooms.ProvisionFile{Name: write.name, Data: write.data, Mode: mode})
	}
	published, err := s.publish(ctx, PublicationRequest{RoomID: roomID, Scope: PublicationShared, Files: names, Payload: files})
	if err != nil {
		rollbackErr := rollbackWrites(roomPath, latest.files, names)
		return ApplyResult{}, wrapApplyError("access list publication", errors.Join(err, rollbackErr))
	}
	observed, err := loadAccessDocument(roomPath)
	if err != nil || observed.revision != preview.NextRevision {
		return ApplyResult{}, wrapApplyError("access list verification", errors.Join(err, &RevisionConflictError{CurrentRevision: observed.revision}))
	}
	return ApplyResult{Revision: observed.revision, Changes: preview.Changes, PublishedTargets: published, Sync: SyncState{
		Status: "synced", Source: "runtime-disk", ObservedRevision: observed.revision,
	}}, nil
}

func (s *Service) accessDocument(ctx context.Context, roomID string) (rooms.Room, string, accessDocument, bool, error) {
	if s.reader == nil {
		room, roomPath, err := s.resolveRoom(roomID)
		if err != nil {
			return rooms.Room{}, "", accessDocument{}, false, err
		}
		document, err := loadAccessDocument(roomPath)
		return room, roomPath, document, false, err
	}
	room, err := s.managedRoom(roomID)
	if err != nil {
		return rooms.Room{}, "", accessDocument{}, true, err
	}
	snapshot, err := s.readRuntimeConfiguration(ctx, roomID, "", string(PublicationShared))
	if err != nil {
		return rooms.Room{}, "", accessDocument{}, true, err
	}
	document, err := loadAccessSnapshot(snapshot.Result.Files)
	return room, "", document, true, err
}

func loadAccessSnapshot(values []shared.RuntimeConfigurationFile) (accessDocument, error) {
	files := make(map[string]fileSnapshot, 3)
	for _, file := range values {
		switch file.Name {
		case "adminlist.txt", "blocklist.txt", "whitelist.txt":
			files[file.Name] = fileSnapshot{data: append([]byte(nil), file.Data...), mode: os.FileMode(file.Mode).Perm(), exists: file.Exists}
		}
	}
	parts := make([]revisionPart, 0, 3)
	result := AccessLists{}
	for _, name := range []string{"adminlist.txt", "blocklist.txt", "whitelist.txt"} {
		file, exists := files[name]
		if !exists {
			return accessDocument{}, errors.New("Runtime 访问名单快照不完整")
		}
		parts = append(parts, revisionPart{name: name, data: file.data, exists: file.exists})
		list, err := decodeAccessList(file.data)
		if err != nil {
			return accessDocument{}, fmt.Errorf("parse %s: %w", name, err)
		}
		switch name {
		case "adminlist.txt":
			result.Admins = list
		case "blocklist.txt":
			result.Blocked = list
		case "whitelist.txt":
			result.Whitelist = list
		}
	}
	result.Revision = revision(parts...)
	return accessDocument{values: result, files: files, revision: result.Revision}, nil
}

func loadAccessDocument(roomPath string) (accessDocument, error) {
	files := make(map[string]fileSnapshot, 3)
	parts := make([]revisionPart, 0, 3)
	values := AccessLists{}
	for _, name := range []string{"adminlist.txt", "blocklist.txt", "whitelist.txt"} {
		data, mode, _, exists, err := readConfiguration(filepath.Join(roomPath, name), true)
		if err != nil {
			return accessDocument{}, err
		}
		files[name] = fileSnapshot{data: data, mode: mode, exists: exists}
		parts = append(parts, revisionPart{name: name, data: data, exists: exists})
		list, err := decodeAccessList(data)
		if err != nil {
			return accessDocument{}, fmt.Errorf("parse %s: %w", name, err)
		}
		switch name {
		case "adminlist.txt":
			values.Admins = list
		case "blocklist.txt":
			values.Blocked = list
		case "whitelist.txt":
			values.Whitelist = list
		}
	}
	values.Revision = revision(parts...)
	return accessDocument{values: values, files: files, revision: values.Revision}, nil
}

func prepareAccessUpdate(roomName string, document accessDocument, request AccessUpdateRequest, enforceConfirmation bool) (Preview, error) {
	next, _, err := normalizedAccessRequest(request)
	if err != nil {
		return Preview{}, err
	}
	changes := accessChanges(document.values, next)
	if len(changes) == 0 {
		return Preview{}, ErrNoChanges
	}
	removals := hasRemovals(document.values.Admins, next.Admins) ||
		hasRemovals(document.values.Blocked, next.Blocked) ||
		hasRemovals(document.values.Whitelist, next.Whitelist)
	if enforceConfirmation && removals && request.Confirmation != roomName {
		return Preview{}, ErrConfirmationNeeded
	}
	parts := accessRevisionParts(document, next)
	return Preview{Revision: document.revision, NextRevision: revision(parts...), Changes: changes, RequiresConfirmation: removals}, nil
}

func normalizedAccessRequest(request AccessUpdateRequest) (AccessLists, bool, error) {
	fields := make(map[string]string)
	admins, err := normalizeAccessList(request.Admins)
	if err != nil {
		fields["admins"] = err.Error()
	}
	blocked, err := normalizeAccessList(request.Blocked)
	if err != nil {
		fields["blocked"] = err.Error()
	}
	whitelist, err := normalizeAccessList(request.Whitelist)
	if err != nil {
		fields["whitelist"] = err.Error()
	}
	if len(fields) > 0 {
		return AccessLists{}, false, &FieldError{Fields: fields}
	}
	return AccessLists{Admins: admins, Blocked: blocked, Whitelist: whitelist}, false, nil
}

func normalizeAccessList(input []string) ([]string, error) {
	if len(input) > 10000 {
		return nil, errors.New("名单不能超过 10000 项")
	}
	seen := make(map[string]bool, len(input))
	result := make([]string, 0, len(input))
	for _, value := range input {
		value = strings.TrimSpace(value)
		if !accessIDPattern.MatchString(value) {
			return nil, errors.New("ID 只能包含字母、数字、下划线和短横线，长度为 1-128")
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result, nil
}

func decodeAccessList(data []byte) ([]string, error) {
	values := strings.Fields(string(data))
	if len(values) == 0 {
		return []string{}, nil
	}
	return normalizeAccessList(values)
}

func encodeAccessList(values []string) []byte {
	if len(values) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(values, "\n") + "\n")
}

func accessChanges(before, after AccessLists) []Change {
	changes := make([]Change, 0, 3)
	items := []struct {
		path, label   string
		before, after []string
	}{
		{"access.admins", "管理员名单", before.Admins, after.Admins},
		{"access.blocked", "封禁名单", before.Blocked, after.Blocked},
		{"access.whitelist", "白名单", before.Whitelist, after.Whitelist},
	}
	for _, item := range items {
		if equalStrings(item.before, item.after) {
			continue
		}
		changes = append(changes, Change{Path: item.path, Label: item.label, Before: item.before, After: item.after, Operation: "replace"})
	}
	return changes
}

func accessWrites(document accessDocument, next AccessLists) []fileWrite {
	writes := make([]fileWrite, 0, 3)
	if !equalStrings(document.values.Admins, next.Admins) {
		writes = append(writes, fileWrite{name: "adminlist.txt", data: encodeAccessList(next.Admins)})
	}
	if !equalStrings(document.values.Blocked, next.Blocked) {
		writes = append(writes, fileWrite{name: "blocklist.txt", data: encodeAccessList(next.Blocked)})
	}
	if !equalStrings(document.values.Whitelist, next.Whitelist) {
		writes = append(writes, fileWrite{name: "whitelist.txt", data: encodeAccessList(next.Whitelist)})
	}
	return writes
}

func accessRevisionParts(document accessDocument, next AccessLists) []revisionPart {
	changed := make(map[string][]byte)
	for _, write := range accessWrites(document, next) {
		changed[write.name] = write.data
	}
	parts := make([]revisionPart, 0, 3)
	for _, name := range []string{"adminlist.txt", "blocklist.txt", "whitelist.txt"} {
		if data, ok := changed[name]; ok {
			parts = append(parts, revisionPart{name: name, data: data, exists: true})
			continue
		}
		snapshot := document.files[name]
		parts = append(parts, revisionPart{name: name, data: snapshot.data, exists: snapshot.exists})
	}
	return parts
}

func hasRemovals(before, after []string) bool {
	present := make(map[string]bool, len(after))
	for _, value := range after {
		present[value] = true
	}
	for _, value := range before {
		if !present[value] {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type fileWrite struct {
	name string
	data []byte
}

func atomicWriteSet(directory string, previous map[string]fileSnapshot, writes []fileWrite) error {
	written := make([]string, 0, len(writes))
	for _, write := range writes {
		snapshot := previous[write.name]
		mode := snapshot.mode
		if mode == 0 {
			mode = 0640
		}
		if err := atomicWrite(filepath.Join(directory, write.name), write.data, mode); err != nil {
			rollbackErr := rollbackWrites(directory, previous, written)
			return errors.Join(err, rollbackErr)
		}
		written = append(written, write.name)
	}
	return nil
}

func rollbackWrites(directory string, previous map[string]fileSnapshot, names []string) error {
	var result error
	for index := len(names) - 1; index >= 0; index-- {
		name := names[index]
		snapshot := previous[name]
		path := filepath.Join(directory, name)
		if snapshot.exists {
			result = errors.Join(result, atomicWrite(path, snapshot.data, snapshot.mode))
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (s *Service) TokenStatus(roomID string) (TokenStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.TokenStatusContext(ctx, roomID)
}

func (s *Service) TokenStatusContext(ctx context.Context, roomID string) (TokenStatus, error) {
	_, _, document, _, err := s.tokenDocument(ctx, roomID)
	return document.status, err
}

func (s *Service) RevealToken(roomID, confirmation string) (TokenReveal, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.RevealTokenContext(ctx, roomID, confirmation)
}

func (s *Service) RevealTokenContext(ctx context.Context, roomID, confirmation string) (TokenReveal, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return TokenReveal{}, err
	}
	if confirmation != room.Name {
		return TokenReveal{}, ErrConfirmationNeeded
	}
	if s.reader != nil {
		if s.tokens == nil {
			return TokenReveal{}, ErrTokenRevealUnavailable
		}
		worldID, err := s.configurationTargetWorld(roomID, "")
		if err != nil {
			return TokenReveal{}, err
		}
		result, err := s.tokens.RevealClusterToken(ctx, roomID, worldID)
		if err != nil {
			return TokenReveal{}, err
		}
		if err := runtimefiles.ValidateClusterTokenReveal(result); err != nil {
			return TokenReveal{}, err
		}
		return TokenReveal{Revision: tokenStatusRevision(result.Exists, result.SHA256), Token: result.Token}, nil
	}
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return TokenReveal{}, err
	}
	data, _, _, exists, err := readConfiguration(filepath.Join(roomPath, "cluster_token.txt"), true)
	if err != nil {
		return TokenReveal{}, err
	}
	token := strings.TrimSpace(string(data))
	return TokenReveal{Revision: tokenStatusRevision(exists, tokenDigest(token)), Token: token}, nil
}

func (s *Service) PreviewToken(roomID string, request TokenUpdateRequest) (Preview, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.PreviewTokenContext(ctx, roomID, request)
}

func (s *Service) PreviewTokenContext(ctx context.Context, roomID string, request TokenUpdateRequest) (Preview, error) {
	room, _, document, _, err := s.tokenDocument(ctx, roomID)
	if err != nil {
		return Preview{}, err
	}
	return prepareTokenUpdate(room, document, request)
}

func (s *Service) ApplyToken(ctx context.Context, jobID, roomID string, request TokenUpdateRequest) (ApplyResult, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer release()
	room, roomPath, document, routed, err := s.tokenDocument(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	preview, err := prepareTokenUpdate(room, document, request)
	if err != nil {
		return ApplyResult{}, err
	}
	_, _, latest, latestRouted, err := s.tokenDocument(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	if latestRouted != routed {
		return ApplyResult{}, &RevisionConflictError{CurrentRevision: latest.status.Revision}
	}
	if err := checkRevision(request.ExpectedRevision, latest.status.Revision); err != nil {
		return ApplyResult{}, err
	}
	data := normalizedTokenData(request.Token)
	mode := latest.mode
	if !latest.exists || mode == 0 {
		mode = 0o600
	}
	if routed {
		if s.publisher == nil {
			return ApplyResult{}, wrapApplyError("cluster token publication", errors.New("configuration publisher is unavailable"))
		}
		published, publishErr := s.publish(ctx, PublicationRequest{
			RoomID: roomID, Scope: PublicationShared, Files: []string{"cluster_token.txt"}, IncludeLocal: true,
			Payload: []rooms.ProvisionFile{{Name: "cluster_token.txt", Data: data, Mode: mode}},
		})
		if publishErr != nil {
			return ApplyResult{}, wrapApplyError("cluster token publication", publishErr)
		}
		return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, PublishedTargets: published, Sync: SyncState{
			Status: "synced", Source: "runtime-disk", ObservedRevision: preview.NextRevision,
		}}, nil
	}
	path := filepath.Join(roomPath, "cluster_token.txt")
	if err := atomicWrite(path, data, mode); err != nil {
		return ApplyResult{}, wrapApplyError("cluster token", err)
	}
	published, err := s.publish(ctx, PublicationRequest{
		RoomID: roomID, Scope: PublicationShared, Files: []string{"cluster_token.txt"},
		Payload: []rooms.ProvisionFile{{Name: "cluster_token.txt", Data: data, Mode: mode}},
	})
	if err != nil {
		previous := map[string]fileSnapshot{"cluster_token.txt": {data: latest.data, mode: latest.mode, exists: latest.exists}}
		rollbackErr := rollbackWrites(roomPath, previous, []string{"cluster_token.txt"})
		return ApplyResult{}, wrapApplyError("cluster token publication", errors.Join(err, rollbackErr))
	}
	observed, err := loadTokenDocument(roomPath)
	if err != nil || observed.status.Revision != preview.NextRevision {
		return ApplyResult{}, wrapApplyError("cluster token verification", errors.Join(err, &RevisionConflictError{CurrentRevision: observed.status.Revision}))
	}
	return ApplyResult{Revision: observed.status.Revision, Changes: preview.Changes, PublishedTargets: published, Sync: SyncState{
		Status: "synced", Source: "runtime-disk", ObservedRevision: observed.status.Revision,
	}}, nil
}

type tokenDocument struct {
	status TokenStatus
	digest string
	data   []byte
	mode   os.FileMode
	exists bool
}

func (s *Service) tokenDocument(ctx context.Context, roomID string) (rooms.Room, string, tokenDocument, bool, error) {
	if s.reader == nil {
		room, roomPath, err := s.resolveRoom(roomID)
		if err != nil {
			return rooms.Room{}, "", tokenDocument{}, false, err
		}
		document, err := loadTokenDocument(roomPath)
		return room, roomPath, document, false, err
	}
	room, err := s.managedRoom(roomID)
	if err != nil {
		return rooms.Room{}, "", tokenDocument{}, true, err
	}
	snapshot, err := s.readRuntimeConfiguration(ctx, roomID, "", runtimefiles.ConfigurationScopeTokenStatus)
	if err != nil {
		return rooms.Room{}, "", tokenDocument{}, true, err
	}
	status := snapshot.Result.TokenStatus
	if status == nil {
		return rooms.Room{}, "", tokenDocument{}, true, errors.New("Runtime 未返回 Cluster Token 状态")
	}
	mode := os.FileMode(status.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	document := tokenDocument{
		digest: status.SHA256, mode: mode, exists: status.Exists,
		status: TokenStatus{
			Revision:   tokenStatusRevision(status.Exists, status.SHA256),
			Configured: status.Configured, MaskedValue: status.MaskedValue,
		},
	}
	return room, "", document, true, nil
}

func loadTokenStatus(roomPath string) (TokenStatus, error) {
	document, err := loadTokenDocument(roomPath)
	return document.status, err
}

func loadTokenDocument(roomPath string) (tokenDocument, error) {
	data, mode, _, exists, err := readConfiguration(filepath.Join(roomPath, "cluster_token.txt"), true)
	if err != nil {
		return tokenDocument{}, err
	}
	token := strings.TrimSpace(string(data))
	digest := tokenDigest(token)
	masked := ""
	if token != "" {
		tail := token
		if len(tail) > 4 {
			tail = tail[len(tail)-4:]
		}
		masked = "****" + tail
	}
	if !exists || mode == 0 {
		mode = 0o600
	}
	return tokenDocument{
		status: TokenStatus{Revision: tokenStatusRevision(exists, digest), Configured: token != "", MaskedValue: masked},
		digest: digest, data: data, mode: mode, exists: exists,
	}, nil
}

func prepareTokenUpdate(room rooms.Room, document tokenDocument, request TokenUpdateRequest) (Preview, error) {
	if request.Confirmation != room.Name {
		return Preview{}, ErrConfirmationNeeded
	}
	if err := rooms.ValidateClusterToken(request.Token, true); err != nil {
		return Preview{}, &FieldError{Fields: map[string]string{"token": err.Error()}}
	}
	currentRevision := document.status.Revision
	if err := checkRevision(request.ExpectedRevision, currentRevision); err != nil {
		return Preview{}, err
	}
	after := strings.TrimSpace(request.Token)
	afterDigest := tokenDigest(after)
	if document.digest == afterDigest {
		return Preview{}, ErrNoChanges
	}
	change := Change{
		Path: "clusterToken", Label: "Cluster Token", Before: configuredLabelFromBool(document.status.Configured), After: configuredLabel(after),
		Sensitive: true, Operation: operation(optionalConfigured(document.status.Configured), optionalSecret(after)),
	}
	return Preview{
		Revision:     currentRevision,
		NextRevision: tokenStatusRevision(true, afterDigest),
		Changes:      []Change{change}, RequiresConfirmation: true,
	}, nil
}

func normalizedTokenData(value string) []byte {
	token := strings.TrimSpace(value)
	if token == "" {
		return []byte{}
	}
	return []byte(token + "\n")
}

func tokenDigest(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

func tokenStatusRevision(exists bool, digest string) string {
	return revision(revisionPart{name: "cluster_token.txt", data: []byte(digest), exists: exists})
}

func configuredLabelFromBool(configured bool) string {
	if configured {
		return "已配置"
	}
	return "未配置"
}

func optionalConfigured(configured bool) interface{} {
	if !configured {
		return nil
	}
	return true
}

func optionalSecret(value string) interface{} {
	if value == "" {
		return nil
	}
	return true
}
