package runtimedriver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"dont/shared"
)

const (
	nativeEvidenceFileName = ".dst-admin-runtime-evidence.json"
	nativeEvidenceLimit    = 256
	nativeEvidenceVersion  = 1
)

type nativeEvidenceDocument struct {
	Version int                               `json:"version"`
	Entries []shared.RuntimeOperationEvidence `json:"entries"`
}

type nativeEvidenceStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]shared.RuntimeOperationEvidence
}

func newNativeEvidenceStore(saveRoot string) *nativeEvidenceStore {
	store := &nativeEvidenceStore{
		path:    filepath.Join(saveRoot, nativeEvidenceFileName),
		entries: make(map[string]shared.RuntimeOperationEvidence),
	}
	store.load()
	return store
}

// load is deliberately tolerant. A damaged evidence file must not prevent the
// controller from starting or the game from being managed.
func (s *nativeEvidenceStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var document nativeEvidenceDocument
	if json.Unmarshal(data, &document) != nil || document.Version != nativeEvidenceVersion {
		return
	}
	for _, entry := range newestNativeEvidence(document.Entries, nativeEvidenceLimit) {
		if strings.TrimSpace(entry.OperationID) == "" || entry.ObservedAt.IsZero() {
			continue
		}
		s.entries[entry.OperationID] = entry
	}
}

func (s *nativeEvidenceStore) observe(operationID string) (shared.RuntimeOperationEvidence, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[operationID]
	return entry, exists
}

func (s *nativeEvidenceStore) remember(entry shared.RuntimeOperationEvidence) error {
	if strings.TrimSpace(entry.OperationID) == "" || entry.ObservedAt.IsZero() {
		return ErrInvalidTarget
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[entry.OperationID] = entry
	entries := make([]shared.RuntimeOperationEvidence, 0, len(s.entries))
	for _, value := range s.entries {
		entries = append(entries, value)
	}
	entries = newestNativeEvidence(entries, nativeEvidenceLimit)
	s.entries = make(map[string]shared.RuntimeOperationEvidence, len(entries))
	for _, value := range entries {
		s.entries[value.OperationID] = value
	}
	return writeNativeEvidenceDocument(s.path, nativeEvidenceDocument{Version: nativeEvidenceVersion, Entries: entries})
}

func newestNativeEvidence(entries []shared.RuntimeOperationEvidence, limit int) []shared.RuntimeOperationEvidence {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ObservedAt.Equal(entries[j].ObservedAt) {
			return entries[i].OperationID < entries[j].OperationID
		}
		return entries[i].ObservedAt.Before(entries[j].ObservedAt)
	})
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries
}

func writeNativeEvidenceDocument(path string, document nativeEvidenceDocument) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".dst-admin-runtime-evidence-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}
