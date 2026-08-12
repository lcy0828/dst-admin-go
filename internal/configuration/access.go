package configuration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dont/internal/roomops"
	"dont/internal/rooms"
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
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return AccessLists{}, err
	}
	defer release()
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return AccessLists{}, err
	}
	document, err := loadAccessDocument(roomPath)
	if err != nil {
		return AccessLists{}, err
	}
	return document.values, nil
}

func (s *Service) PreviewAccess(roomID string, request AccessUpdateRequest) (Preview, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return Preview{}, err
	}
	defer release()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return Preview{}, err
	}
	document, err := loadAccessDocument(roomPath)
	if err != nil {
		return Preview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return Preview{}, err
	}
	return prepareAccessUpdate(room.Name, document, request, false)
}

func (s *Service) ValidateAccessApply(roomID string, request AccessUpdateRequest) (Preview, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return Preview{}, err
	}
	defer release()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return Preview{}, err
	}
	document, err := loadAccessDocument(roomPath)
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
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	document, err := loadAccessDocument(roomPath)
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
	backup, err := s.protectionBackup(ctx, room, "访问名单", jobID)
	if err != nil {
		return ApplyResult{}, wrapApplyError("access lists", err)
	}
	latest, err := loadAccessDocument(roomPath)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, latest.revision); err != nil {
		return ApplyResult{}, err
	}
	next, _, err := normalizedAccessRequest(request)
	if err != nil {
		return ApplyResult{}, err
	}
	writes := accessWrites(latest, next)
	if err := atomicWriteSet(roomPath, latest.files, writes); err != nil {
		return ApplyResult{}, wrapApplyError("access lists", err)
	}
	return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, ProtectionBackupID: backup.ID}, nil
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
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return TokenStatus{}, err
	}
	defer release()
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return TokenStatus{}, err
	}
	return loadTokenStatus(roomPath)
}

func (s *Service) RevealToken(roomID, confirmation string) (TokenReveal, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return TokenReveal{}, err
	}
	defer release()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return TokenReveal{}, err
	}
	if confirmation != room.Name {
		return TokenReveal{}, ErrConfirmationNeeded
	}
	data, _, _, exists, err := readConfiguration(filepath.Join(roomPath, "cluster_token.txt"), true)
	if err != nil {
		return TokenReveal{}, err
	}
	return TokenReveal{Revision: revision(revisionPart{name: "cluster_token.txt", data: data, exists: exists}), Token: strings.TrimSpace(string(data))}, nil
}

func (s *Service) PreviewToken(roomID string, request TokenUpdateRequest) (Preview, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return Preview{}, err
	}
	defer release()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return Preview{}, err
	}
	return prepareTokenUpdate(room, roomPath, request)
}

func (s *Service) ApplyToken(ctx context.Context, jobID, roomID string, request TokenUpdateRequest) (ApplyResult, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer release()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	preview, err := prepareTokenUpdate(room, roomPath, request)
	if err != nil {
		return ApplyResult{}, err
	}
	backup, err := s.protectionBackup(ctx, room, "Cluster Token", jobID)
	if err != nil {
		return ApplyResult{}, wrapApplyError("cluster token", err)
	}
	status, err := loadTokenStatus(roomPath)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, status.Revision); err != nil {
		return ApplyResult{}, err
	}
	token := strings.TrimSpace(request.Token)
	data := []byte{}
	if token != "" {
		data = []byte(token + "\n")
	}
	path := filepath.Join(roomPath, "cluster_token.txt")
	_, mode, _, _, err := readConfiguration(path, true)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := atomicWrite(path, data, mode); err != nil {
		return ApplyResult{}, wrapApplyError("cluster token", err)
	}
	return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, ProtectionBackupID: backup.ID}, nil
}

func loadTokenStatus(roomPath string) (TokenStatus, error) {
	data, _, _, exists, err := readConfiguration(filepath.Join(roomPath, "cluster_token.txt"), true)
	if err != nil {
		return TokenStatus{}, err
	}
	token := strings.TrimSpace(string(data))
	masked := ""
	if token != "" {
		tail := token
		if len(tail) > 4 {
			tail = tail[len(tail)-4:]
		}
		masked = "****" + tail
	}
	return TokenStatus{Revision: revision(revisionPart{name: "cluster_token.txt", data: data, exists: exists}), Configured: token != "", MaskedValue: masked}, nil
}

func prepareTokenUpdate(room rooms.Room, roomPath string, request TokenUpdateRequest) (Preview, error) {
	if request.Confirmation != room.Name {
		return Preview{}, ErrConfirmationNeeded
	}
	if err := rooms.ValidateClusterToken(request.Token, true); err != nil {
		return Preview{}, &FieldError{Fields: map[string]string{"token": err.Error()}}
	}
	data, _, _, exists, err := readConfiguration(filepath.Join(roomPath, "cluster_token.txt"), true)
	if err != nil {
		return Preview{}, err
	}
	currentRevision := revision(revisionPart{name: "cluster_token.txt", data: data, exists: exists})
	if err := checkRevision(request.ExpectedRevision, currentRevision); err != nil {
		return Preview{}, err
	}
	before := strings.TrimSpace(string(data))
	after := strings.TrimSpace(request.Token)
	if before == after {
		return Preview{}, ErrNoChanges
	}
	next := []byte{}
	if after != "" {
		next = []byte(after + "\n")
	}
	change := Change{
		Path: "clusterToken", Label: "Cluster Token", Before: configuredLabel(before), After: configuredLabel(after),
		Sensitive: true, Operation: operation(optionalSecret(before), optionalSecret(after)),
	}
	return Preview{
		Revision:     currentRevision,
		NextRevision: revision(revisionPart{name: "cluster_token.txt", data: next, exists: true}),
		Changes:      []Change{change}, RequiresConfirmation: true,
	}, nil
}

func optionalSecret(value string) interface{} {
	if value == "" {
		return nil
	}
	return true
}
