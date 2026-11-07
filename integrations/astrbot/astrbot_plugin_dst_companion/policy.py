from dataclasses import dataclass, field

from .gateway import QueryError

READS = frozenset(
    {
        "api_listRoomRecentEvents",
        "api_listBotCapabilities",
        "api_queryBotCapability",
        "api_getBotCapabilityResult",
        "api_listRooms",
        "api_listRoomWorlds",
        "api_listWorldStates",
        "api_listRoomPlayers",
        "api_resolveRoomPlayer",
        "api_getRoomPlayerInventory",
        "api_getRoomPerformanceReport",
        "api_listRoomMods",
        "api_getRoomTeleportPreview",
    }
)
WRITES = frozenset(
    {"api_runBotCapability", "api_runRoomAction", "api_createRoomBackup", "api_kickRoomPlayer", "api_executeRoomTeleport", "api_cancelRoomTeleport"}
)


@dataclass(frozen=True)
class Actor:
    key: tuple
    scope: tuple
    user: str
    role: str
    rooms: tuple
    visibility: frozenset = field(
        default_factory=lambda: frozenset(
            {"status", "players", "profile", "inventory", "network", "help", "mods"}
        )
    )
    event: object = field(default=None, compare=False, repr=False)

    write_allowlist: tuple = (
        "room.backup.create",
        "room.start",
        "room.stop",
        "room.restart",
        "player.kick",
    )
    config_revision: str = ""
    page_size: int = 5
    directory_ttl: int = 60
    connection_id: str = ""
    disabled_capabilities: tuple = ()
    workbench_mode: str = "off"
    group_role: str = ""
    catalog_all: bool = False

    def may_write(self, capability):
        """Administrators run every write under the simple policy ("*")."""
        return self.role in {"game-manage", "panel-manage"} and (
            "*" in self.write_allowlist or capability in self.write_allowlist
        )

    @property
    def read_projection(self):
        return (
            self.scope,
            self.role,
            self.rooms,
            tuple(sorted(self.visibility)),
            self.write_allowlist,
            self.workbench_mode,
            self.config_revision,
            self.group_role,
            self.catalog_all,
            tuple(sorted(self.disabled_capabilities)),
        )

    @property
    def projection(self):
        return (
            self.key,
            self.role,
            self.rooms,
            tuple(sorted(self.visibility)),
            self.write_allowlist,
            self.workbench_mode,
            self.config_revision,
            self.group_role,
            self.catalog_all,
            tuple(sorted(self.disabled_capabilities)),
        )


class Policy:
    def __init__(self, native):
        self.native = native
        self.directory = None

    async def authorize(self, actor, name, args):
        if name == "companion_gateway":
            from .generic import actor_envelope
            if args.get("actor") != actor_envelope(actor):
                raise QueryError("PERMISSION_DENIED")
            if args.get("mode") in {"run", "execute"} and actor.role not in {"game-manage", "panel-manage"}:
                raise QueryError("PERMISSION_DENIED")
            if not await self.native(actor, "companion_gateway_write" if args.get("mode") in {"run", "execute"} else name):
                raise QueryError("NATIVE_PERMISSION_DENIED")
            return
        if (
            name not in READS
            and not (
                actor.role in ("game-manage", "panel-manage")
                and (
                    name in WRITES
                    or name
                    in {
                        "api_getJob",
                        "get_job",
                        "call_result",
                        "api_listRoomBackupSchedules",
                    }
                )
            )
            and not (actor.role == "panel-manage" and name in {"find_api", "call_api"})
        ):
            raise QueryError("PERMISSION_DENIED")
        room = args.get("path", {}).get("roomId") or args.get("roomId")
        if name == "call_api":
            room = args.get("arguments", {}).get("path", {}).get("roomId")
            if not room:
                raise QueryError("ROOM_DENIED")
        if room and room not in actor.rooms:
            raise QueryError("ROOM_DENIED")
        if name in {"api_queryBotCapability", "api_runBotCapability"}:
            from .capabilities import permitted

            item = (
                self.directory.lookup(actor, args.get("path", {}).get("capabilityId"))
                if self.directory
                else None
            )
            if (
                not item
                or not permitted(actor, item, args.get("body", {}).get("parameters") if name=="api_runBotCapability" else None)
                or (name == "api_runBotCapability") != (item["access"] == "write")
            ):
                raise QueryError("PERMISSION_DENIED")
            for underlying in item["nativeTools"]:
                if not await self.native(actor, underlying):
                    raise QueryError("NATIVE_PERMISSION_DENIED")
        if name in {"api_executeRoomTeleport", "api_cancelRoomTeleport", "api_getRoomTeleportPreview"}:
            from .capabilities import permitted
            item = self.directory.lookup(actor, "player.teleport") if self.directory else None
            if not item or not permitted(actor, item):
                raise QueryError("PERMISSION_DENIED")
        # Native policy can be stricter than the companion policy.
        if not await self.native(actor, name):
            raise QueryError("NATIVE_PERMISSION_DENIED")

    async def authorize_receipt(self, actor, item):
        from .capabilities import permitted

        if not permitted(actor, item):
            raise QueryError("PERMISSION_DENIED")
        for tool in ["api_getBotCapabilityResult", *item["nativeTools"]]:
            if not await self.native(actor, tool):
                raise QueryError("NATIVE_PERMISSION_DENIED")
