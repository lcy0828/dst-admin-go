"""Presentation for server-owned teleport previews; no executable templates."""
from datetime import datetime
from .formatters import safe

REASONS = {
    "PLAYER_OFFLINE": "玩家已离线，请等玩家进入游戏后再试。",
    "PLAYER_MOVING_WORLD": "玩家正在切换世界，请稍后重新检查。",
    "PLAYER_GHOST": "玩家目前是幽灵，请先复活。",
    "PLAYER_RIDING": "请让玩家先下坐骑。",
    "PLAYER_ON_BOAT": "请让玩家先离开船只。",
    "PLAYER_TRANSFORMED": "请让玩家恢复普通形态。",
    "WORLD_MAINTENANCE": "世界正在备份或维护，请结束后重新检查。",
    "NO_LANDING_POINT": "附近没有可落脚的位置，请换一个地点。",
    "LANDING_CHANGED": "目的地已出现障碍，请重新检查。",
    "LOCATION_CHANGED": "地点已修改或删除，请重新选择。",
    "TARGET_MOVED": "目标玩家已离开原位置，请重新检查。",
    "WORLD_CHANGED": "世界状态已变化，请重新检查。",
    "PREVIEW_EXPIRED": "确认已过期，请重新检查。",
    "TELEPORT_BUSY": "玩家还有待处理传送，请稍后再试。",
    "TELEPORT_CPU_LIMIT": "检查耗时较长，尚未传送，请稍后重试。",
    "TELEPORT_SCAN_LIMIT": "目的地附近对象较多，请选择更开阔的位置。",
}


def remaining(data):
    try:
        expires = datetime.fromisoformat(data["expiresAt"].replace("Z", "+00:00"))
        server = datetime.fromisoformat(data["serverTime"].replace("Z", "+00:00"))
        return max(0, min(60, (expires - server).total_seconds()))
    except (ValueError, TypeError, KeyError):
        return 0


def describe(data):
    state = data.get("state")
    label = {"prepared": "📍 目的地已检查", "confirmed": "✅ 已传送，位置已确认",
             "rejected": "未执行传送", "cancelled": "已取消，尚未执行", "expired": "确认已过期",
             "dispatching": "正在等待原传送结果", "uncertain": "尚未确认是否传送，请查看原结果，不要重复操作"}.get(state, "尚未取得传送结果")
    name = safe(data.get("playerName") or data.get("playerId") or "玩家")
    point = safe(data.get("locationName") or "选定位置")
    reason = REASONS.get(data.get("reason"))
    return (f"{label}\n{name} → {point}\nX {safe(data.get('x'))} / Z {safe(data.get('z'))}"
            + ("\n" + reason if reason else "")
            + ("\n世界存档发生过变化，请确认基地等建筑仍在原处。" if data.get("needsReview") else ""))


def envelope(data):
    state = data.get("state")
    return {"capabilityId": "player.teleport", "resultKind": "facts", "data": data,
            "status": "succeeded" if state == "confirmed" else "failed" if state in {"rejected", "expired", "cancelled"} else "uncertain",
            "complete": state in {"confirmed", "rejected", "expired", "cancelled"},
            "executionOutcome": "confirmed" if state == "confirmed" else "unknown",
            "mayHaveExecuted": state in {"dispatching", "uncertain"}}


def describe_movement(data):
    items = data.get("items") or []
    labels = {"preview":"📍 已检查本次玩家与落点，尚未传送", "confirmed":"✅ 传送位置已确认", "partial":"部分玩家未完成或位置未确认，请勿重复提交", "cancelled":"已取消本次传送确认"}
    rows = [labels.get(data.get("state"), "传送结果尚未确认")]
    for item in items[:40]:
        p = item.get("point") or {}
        status = {"prepared":"待确认", "confirmed":"已确认", "uncertain":"结果待确认", "rejected":"未执行"}.get(item.get("state"),"待核对")
        rows.append(f"· {safe(item.get('playerName') or item.get('playerId'))} → X {safe(p.get('x'))} / Z {safe(p.get('z'))} · {status}")
    if not items: rows.append("本次没有需要移动的玩家。")
    return "\n".join(rows)
