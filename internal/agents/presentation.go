package agents

import (
	"net"
	"os"
	"strings"
)

// SetRuntimeTargetDisplayAddress changes presentation metadata only. Runtime
// routing and player/shard network profiles deliberately remain independent.
func (s *Service) SetRuntimeTargetDisplayAddress(targetID, address string) (RuntimeTarget, error) {
	targetID, address = strings.TrimSpace(targetID), strings.TrimSpace(address)
	if !validDisplayAddress(address) {
		return RuntimeTarget{}, ErrInvalidInput
	}
	target, err := s.resolvePresentationTarget(targetID)
	if err != nil {
		return RuntimeTarget{}, err
	}
	if err := s.store.SaveNodeDisplayAddress(targetID, address); err != nil {
		return RuntimeTarget{}, err
	}
	target.DisplayAddress = address
	return target, nil
}

func validDisplayAddress(address string) bool {
	if address == "" {
		return true
	}
	if ip := net.ParseIP(address); ip != nil {
		return !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsMulticast()
	}
	if len(address) > 253 || strings.EqualFold(address, "localhost") {
		return false
	}
	for _, label := range strings.Split(address, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return strings.IndexFunc(address, func(c rune) bool { return c != '.' && (c < '0' || c > '9') }) >= 0
}

func (s *Store) SaveNodeDisplayAddress(targetID, address string) error {
	s.nodeMetadataMu.Lock()
	defer s.nodeMetadataMu.Unlock()
	now := s.now().UTC()
	var count int
	if err := s.db.Table(s.nodeNamesTable).Where("target_id = ?", targetID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return s.db.Table(s.nodeNamesTable).Create(&nodeDisplayNameRecord{TargetID: targetID, DisplayAddress: address, CreatedAt: now, UpdatedAt: now}).Error
	}
	return s.db.Table(s.nodeNamesTable).Where("target_id = ?", targetID).Updates(map[string]interface{}{"display_address": address, "updated_at": now}).Error
}

func localRuntimeContainerized() bool {
	for _, path := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}
