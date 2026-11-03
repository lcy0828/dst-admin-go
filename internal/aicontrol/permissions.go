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
	"listRoomLocations": true, "getRoomLocation": true, "getRoomTeleportPreview": true,
	"getRoomRuntimeOverview": true, "listBotCapabilities": true, "queryBotCapability": true, "getBotCapabilityResult": true, "listRooms": true, "listRoomWorlds": true, "listWorldStates": true,
	"listRoomPlayers": true, "resolveRoomPlayer": true, "getRoomPlayerInventory": true,
	"getRoomPerformanceReport": true, "listRoomMods": true, "searchWorldEntityCatalog": true,
	"getWorldEntityArtwork": true, "getWorldStateHistory": true,
	"queryRoomWorld": true, "getRoomWorldQueryRun": true,
	"listRoomPlayerActivity": true, "getRoomPlayerTrajectory": true, "getRoomPlayerCombat": true,
}
var gameWrites = map[string]bool{
	"createRoomLocation": true, "updateRoomLocation": true, "deleteRoomLocation": true, "prepareRoomTeleport": true, "executeRoomTeleport": true, "cancelRoomTeleport": true,
	"runBotCapability": true, "runRoomAction": true, "createRoomBackup": true,
	"configureRoomBackupSchedule": true, "listRoomBackupSchedules": true,
	"kickRoomPlayer": true,
	// Fixed player actions belong to game management, independently of which
	// API/MCP entry point the client discovers. Raw Lua remains panel-only.
	"setRoomPlayerGodMode": true, "setRoomPlayerCreativeMode": true,
	"resurrectRoomPlayer": true, "killRoomPlayer": true, "resetRoomPlayerCharacter": true,
	"banRoomPlayer": true, "unbanRoomPlayer": true, "announceToRoomPlayer": true,
	"getRoomPlayer": true, "refreshRoomPlayers": true, "listRoomBackups": true,
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
	if id == "getBotProtocol" || id == "queryBotTask" || id == "getBotTask" {
		return true
	}
	if id == "prepareBotTask" || id == "executeBotTask" {
		return p.Profile == "" || p.Profile == ProfilePanelManage || p.Profile == ProfileGameManage
	}
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
	if strings.HasPrefix(name, apicontract.ToolPrefix) {
		id := strings.TrimPrefix(name, apicontract.ToolPrefix)
		if id == "getBotProtocol" || id == "queryBotTask" || id == "getBotTask" || id == "prepareBotTask" || id == "executeBotTask" {
			return p.AllowsOperation(id, "")
		}
	}
	if write, ok := taskNames[name]; ok {
		return !write || p.Profile == "" || p.Profile == ProfilePanelManage || p.Profile == ProfileGameManage
	}
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
	return p.CheckAPI(op, in) == nil
}

func (p Principal) CheckAPI(op *apicontract.Operation, in apicontract.Input) error {
	room, _ := in.Path["roomId"].(string)
	if !p.AllowsOperation(op.ID, room) {
		return ErrDenied
	}
	if op.ID == "listRoomPlayers" {
		_, err := p.playerListAccess(in)
		return err
	}
	if p.Profile == ProfileGameRead || p.Profile == ProfileGameManage {
		if op.ID == "runRoomAction" {
			action, _ := in.Path["action"].(string)
			if !slices.Contains([]string{"start", "stop", "restart", "save"}, action) {
				return ErrDenied
			}
		}
	}
	return nil
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
