package aicontrol

import (
	"dont/internal/apicontract"
	"slices"
	"strings"
)

const (
	ProfileGameRead    = "game-read"
	ProfileGameManage  = "game-manage"
	ProfilePanelManage = "panel-manage"
)

// These are executable policy, not metadata or a listing filter. Unknown
// operations fail closed. Existing unscoped tokens retain panel administration.
var gameReads = map[string]bool{
	"getRoomRuntimeOverview": true, "listBotCapabilities": true, "queryBotCapability": true, "getBotCapabilityResult": true, "listRooms": true, "listRoomWorlds": true, "listWorldStates": true,
	"listRoomPlayers": true, "resolveRoomPlayer": true, "getRoomPlayerInventory": true,
	"getRoomPerformanceReport": true, "listRoomMods": true, "searchWorldEntityCatalog": true,
	"getWorldEntityArtwork": true, "getWorldStateHistory": true,
	"queryRoomWorld": true, "getRoomWorldQueryRun": true,
}
var gameWrites = map[string]bool{
	"runBotCapability": true, "runRoomAction": true, "createRoomBackup": true,
	"configureRoomBackupSchedule": true, "listRoomBackupSchedules": true,
	"kickRoomPlayer":     true,
	"giveRoomPlayerItem": true, "getRoomPlayerItemGrant": true,
	"spawnRoomPlayerEntity": true, "getRoomPlayerEntitySpawn": true,
	"executeBuiltinRoomCommand": true, "getBuiltinRoomCommandRun": true,
}

func (p Principal) Restricted() bool {
	return p.Profile != "" && p.Profile != ProfilePanelManage || p.RoomID != "" || len(p.RoomIDs) > 0
}
func (p Principal) AllowsRoom(room string) bool {
	if p.RoomID != "" && p.RoomID != room {
		return false
	}
	if len(p.RoomIDs) > 0 {
		return slices.Contains(p.RoomIDs, room)
	}
	return p.Profile == "" || p.Profile == ProfilePanelManage
}
func (p Principal) AllowsOperation(id, room string) bool {
	if room != "" && !p.AllowsRoom(room) {
		return false
	}
	if p.Profile == "" || p.Profile == ProfilePanelManage {
		return !p.Restricted() || room != "" || id == "listRooms" || id == "listBotCapabilities" || id == "getJob"
	}
	if id == "listRooms" || id == "listBotCapabilities" {
		return true
	}
	if id == "getJob" {
		return p.Profile == ProfileGameManage
	} // resource ownership checked by HTTP dispatcher
	if room == "" {
		return false
	}
	return gameReads[id] || p.Profile == ProfileGameManage && gameWrites[id]
}
func (p Principal) AllowsTool(name string) bool {
	if p.Profile == "" || p.Profile == ProfilePanelManage {
		return true
	}
	if strings.HasPrefix(name, apicontract.ToolPrefix) {
		id := strings.TrimPrefix(name, apicontract.ToolPrefix)
		return gameReads[id] || p.Profile == ProfileGameManage && (gameWrites[id] || id == "getJob")
	}
	return name == discoverAPI || name == dispatchAPI || name == "list_rooms" || name == "call_result" || p.Profile == ProfileGameManage && (name == "get_job" || name == "room_control")
}
func (p Principal) AllowsAPI(op *apicontract.Operation, in apicontract.Input) bool {
	room, _ := in.Path["roomId"].(string)
	if !p.AllowsOperation(op.ID, room) {
		return false
	}
	if p.Profile == ProfileGameRead || p.Profile == ProfileGameManage {
		// The public projection excludes access lists; being a GET is insufficient.
		if op.ID == "listRoomPlayers" && in.Query["includeAccessLists"] != false {
			return false
		}
		if op.ID == "runRoomAction" {
			action, _ := in.Path["action"].(string)
			if !slices.Contains([]string{"start", "stop", "restart", "save"}, action) {
				return false
			}
		}
	}
	return true
}
func normalizeTokenScope(input *TokenInput) error {
	if input.Profile == "" {
		input.Profile = ProfilePanelManage
	}
	if !slices.Contains([]string{ProfileGameRead, ProfileGameManage, ProfilePanelManage}, input.Profile) || len(input.RoomIDs) > 64 {
		return ErrInvalid
	}
	for _, room := range input.RoomIDs {
		if room == "" || len(room) > 255 || strings.ContainsAny(room, "\x00\r\n") {
			return ErrInvalid
		}
	}
	slices.Sort(input.RoomIDs)
	input.RoomIDs = slices.Compact(input.RoomIDs)
	if input.Profile != ProfilePanelManage && len(input.RoomIDs) == 0 {
		return ErrInvalid
	}
	return nil
}
