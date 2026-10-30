"""Fixed capabilities shared by parsing, menus and execution authorization.

This is plugin UX metadata. The panel's executable policy remains authoritative;
neither configuration nor model output can register another operation here.
"""

from dataclasses import dataclass


@dataclass(frozen=True)
class Command:
    kind: str
    label: str
    phrases: tuple[str, ...]
    example: str
    visibility: str = ""
    management: bool = False
    player_required: bool = False

    def available(self, actor):
        ids = {
            "status": "room.status",
            "players": "room.players",
            "profile": "player.resolve",
            "network": "room.network",
            "inventory": "player.inventory",
            "mods": "room.mods",
            "backups": "room.backups.settings",
            "backup": "room.backup.create",
            "start": "room.start",
            "stop": "room.stop",
            "restart": "room.restart",
            "kick": "player.kick",
        }
        if ids.get(self.kind) in actor.disabled_capabilities:
            return False
        if self.management:
            if actor.role not in {"game-manage", "panel-manage"}:
                return False
            writes = {
                "backup": "room.backup.create",
                "start": "room.start",
                "stop": "room.stop",
                "restart": "room.restart",
                "kick": "player.kick",
            }
            return self.kind not in writes or writes[self.kind] in actor.write_allowlist
        return (self.visibility or self.kind) in actor.visibility


COMMANDS = (
    Command(
        "rooms",
        "服务器列表",
        (
            "有哪些服",
            "有什么服",
            "有哪些服务器",
            "有哪些房间",
            "服务器列表",
            "房间列表",
            "所有服务器",
            "所有房间",
        ),
        "有哪些服",
        visibility="status",
    ),
    Command(
        "status",
        "房间状态",
        ("查房", "服务器状态", "房间状态", "运行状态", "状态", "信息", "怎么样", "运行正常吗"),
        "1服状态",
    ),
    Command(
        "players",
        "在线玩家",
        ("谁在线", "有哪些人", "有哪些玩家", "在线玩家", "玩家列表", "玩家", "现在谁在线"),
        "1服谁在线",
    ),
    Command(
        "profile",
        "玩家信息",
        ("查玩家", "他在哪", "他在哪里", "查他", "查我", "我在哪"),
        "查玩家 昵称",
        player_required=True,
    ),
    Command(
        "inventory",
        "玩家背包",
        ("查背包", "背包", "查他的背包", "查他背包"),
        "查背包 昵称",
        player_required=True,
    ),
    Command(
        "network",
        "网络档位",
        ("延迟", "谁卡", "网络", "网络质量", "网络连接质量", "查网络", "查他网络"),
        "1服网络",
    ),
    Command(
        "mods", "模组列表", ("模组", "模组列表", "安装了哪些模组", "装了哪些模组"), "1服模组列表"
    ),
    Command(
        "backups",
        "备份设置",
        ("自动备份开了吗", "备份开了吗", "备份状态"),
        "1服备份状态",
        management=True,
    ),
    Command("backup", "立即备份", ("立即备份", "备份一次"), "1服立即备份", management=True),
    Command("start", "启动房间", ("启动", "启动房间"), "启动1服", management=True),
    Command("stop", "停止房间", ("停止", "停止房间"), "停止1服", management=True),
    Command("restart", "重启房间", ("重启", "重启房间"), "重启1服", management=True),
    Command("kick", "踢出玩家", ("踢出",), "1服踢出 昵称", management=True, player_required=True),
    Command(
        "job", "任务进度", ("任务进度", "备份进度", "上次任务"), "1服任务进度", management=True
    ),
    Command(
        "stop_tracking",
        "停止任务通知",
        ("算了", "别回复了", "停止跟踪"),
        "停止跟踪",
        management=True,
    ),
    Command("help", "菜单", ("帮助", "菜单", "能做什么", "你能做什么", "有哪些功能"), "菜单"),
    Command("cancel", "取消选择", ("取消", "返回", "取消选择"), "取消", visibility="help"),
)
BY_KIND = {command.kind: command for command in COMMANDS}
BY_PHRASE = {phrase: command for command in COMMANDS for phrase in command.phrases}


def menu_commands(actor):
    return [
        c
        for c in COMMANDS
        if c.kind not in {"help", "cancel", "stop_tracking"} and c.available(actor)
    ]


def allowed(actor, kind):
    command = BY_KIND.get(kind)
    return command is not None and command.available(actor)
