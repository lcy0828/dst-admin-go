"""Transfer the legacy broadcaster only after durable ownership is recorded."""

import copy

from .broadcast import READ_TOOLS
from .connections import endpoint


def legacy_instance(plugin, metadata=None):
    if metadata is None:
        getter = getattr(plugin.context, "get_registered_star", None)
        metadata = getter("astrbot_plugin_dst_broadcast") if getter else None
    return getattr(metadata, "star_cls", None)


def legacy_session(legacy):
    config = getattr(legacy, "config", {})
    group, platform = str(config.get("target_group_id", "")), str(config.get("platform_id", ""))
    if not group.isdigit() or not platform or ":" in platform:
        return ""
    return f"{platform}:GroupMessage:{group}"


def legacy_owns(plugin, session):
    legacy = legacy_instance(plugin)
    # While an enabled legacy instance exists, never create a second sender
    # for its group, including during a failed or concurrent migration.
    return bool(
        legacy
        and getattr(legacy, "config", {}).get("enabled")
        and legacy_session(legacy) == session
    )


async def migrate_broadcast(plugin, metadata=None):
    async with plugin.migration_lock:
        await _migrate(plugin, metadata)


async def _migrate(plugin, metadata):
    legacy = legacy_instance(plugin, metadata)
    config = getattr(legacy, "config", None)
    if legacy is None or not isinstance(config, dict):
        return
    session = legacy_session(legacy)
    existing = plugin.enrollment.records.get(session)
    pending = existing and existing.get("noticeOwner") == "pending"
    if not session or not config.get("enabled", False) and not pending:
        return
    save = getattr(config, "save_config_async", None)
    if save is None:
        plugin.logger.warning("旧播报配置不支持可靠保存，保持原插件运行；未启动重复播报。")
        return
    try:
        source = plugin.connections.source()
        server = str(config.get("mcp_server", "dst-admin"))
        origin = endpoint(plugin.connections.configuration(plugin.server(False)))
        if not origin or origin != endpoint(plugin.connections.configuration(server)):
            plugin.logger.warning("旧播报连接属于其他面板，保留原实例。")
            return
        rooms = [str(config[k]) for k in ("room_id", "room_id_2") if config.get(k)]
        if not rooms or not plugin.session_native_allowed(session):
            return
        if existing and (existing.get("source") != source or not existing.get("enabled")):
            return  # Never override an explicit opt-out or another panel binding.
        await plugin.catalog.refresh()
        if not set(rooms) <= {r["id"] for r in plugin.catalog.rows}:
            plugin.logger.warning("旧播报房间不在当前密钥范围，未自动迁移。")
            return
        if not plugin.connections.broadcast(READ_TOOLS):
            plugin.logger.warning("等待同源播报读取能力后再迁移旧播报。")
            return
        if existing is None:
            record = {
                "enabled": True,
                "query": session in plugin.config.get("allowed_sessions", []),
                "source": source,
                "rooms": rooms,
                "notices": {
                    k: bool(config.get(k, k != "chat")) for k in ("presence", "server", "chat")
                },
            }
        else:
            record = copy.deepcopy(existing)
        record["noticeOwner"] = "pending"
        # Persist the exclusion first. A crash at any following instruction
        # leaves either the legacy owner, or a resumable pending transfer.
        await plugin.enrollment.update(session, record)
        await plugin.notifications.reconcile()
        await legacy.terminate()
        try:
            state = copy.deepcopy(await legacy.get_kv_data("broadcast_state_v1", {}))
            if not pending:
                await plugin.put_kv_data(
                    "legacy_broadcast_backup_v1",
                    {"config": copy.deepcopy(dict(config)), "state": state},
                )
            # Attach routing to old server/chat notices. Do not infer a room
            # when the old key cannot be matched unambiguously.
            for notice in state.get("outbox", []):
                if notice.get("related"):
                    continue
                key = str(notice.get("key", ""))
                for room in rooms:
                    for prefix, kind in (
                        (room + "/", "player"),
                        ("audit/" + room + "/", "server"),
                        ("chat/" + room + "/", "chat"),
                    ):
                        if key.startswith(prefix):
                            notice["related"] = {"roomId": room, "kind": kind}
            await plugin.notifications.save(session, source, state)
            config["enabled"] = False
            if await save() is not True:
                raise ValueError("legacy configuration save was superseded")
            record["noticeOwner"] = "unified"
            await plugin.enrollment.update(session, record)
            plugin.logger.info("已合并旧事件播报；游标和未知投递状态保留，原插件已关闭。")
        except BaseException:
            # A failed transfer never activates the new sender. Restore the
            # old owner only if its enabled state has been persisted again.
            config["enabled"] = True
            if await save() is True:
                await legacy.initialize()
            raise
    except Exception:
        plugin.logger.warning("旧播报迁移尚未确认；保留单一投递者，可在插件重载时继续迁移。")
