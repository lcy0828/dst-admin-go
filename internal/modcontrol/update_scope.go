package modcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"dont/internal/mods"
)

// Only configuration and placement belong in this revision. Inventory timestamps
// and downloaded versions change during execution and must not invalidate it.
func (s *Service) ModUpdateScope(ctx context.Context, roomID string) (mods.ModUpdateScope, error) {
	affected, err := s.AffectedRoomIDs(ctx, roomID)
	if err != nil {
		return mods.ModUpdateScope{}, err
	}
	snapshot, err := s.source.load(withPublicationSnapshot(ctx, proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true}))
	if err != nil {
		return mods.ModUpdateScope{}, err
	}
	positions := map[string]string{}
	for _, id := range affected {
		if id != roomID {
			related, err := s.source.load(withPublicationSnapshot(ctx, proposal{roomID: id, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true}))
			if err != nil {
				return mods.ModUpdateScope{}, err
			}
			for key, content := range related.contents {
				snapshot.contents[key] = content
			}
		}
		executions, err := s.source.topology.ResolveRoomExecutions(ctx, id)
		if err != nil {
			return mods.ModUpdateScope{}, err
		}
		for _, execution := range executions {
			placement := publicationPlacement(execution, id, execution.World.ID)
			encoded, err := json.Marshal(placement)
			if err != nil {
				return mods.ModUpdateScope{}, err
			}
			positions[id+"\x00"+execution.World.ID] = execution.Revision + "\x00" + string(encoded)
		}
	}
	encoded, err := json.Marshal(struct {
		Contents  map[string][]byte
		Positions map[string]string
	}{snapshot.contents, positions})
	if err != nil {
		return mods.ModUpdateScope{}, err
	}
	digest := sha256.Sum256(encoded)
	return mods.ModUpdateScope{Revision: hex.EncodeToString(digest[:]), AffectedRoomIDs: affected}, nil
}
