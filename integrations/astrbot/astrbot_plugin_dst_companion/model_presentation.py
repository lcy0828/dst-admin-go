"""Per-event display receipts for explicit single queries, never model-authored data."""

import hashlib
import weakref
from collections import OrderedDict
from dataclasses import dataclass, field

from .routing import READ_KINDS, parse, read_request_intent

STYLE = (
    "\n[DST 回复排版；仅用于饥荒查询与操作，不改变其他话题的人格] "
    "插件工具返回的查询卡片已按群模板排版，保留标题、图标、换行、来源、采样与缺失说明；"
    "如果工具返回含 result.data 的JSON，必须从其中提取问题所需的事实，不能只复述reply中的执行完成提示。"
    "记录时间使用同一记录的timeEvidence北京时间；查询范围的起止时间不能代替事件发生时间。"
    "战斗统计以combatEvidence为准，kills为0表示展示记录没有击杀，不能说有击杀记录；unknownDamageHits不换算为伤害。"
    "单项查询直接给出卡片，不要改写成散文、重复菜单或增加“来啦/我先查询”等开场。"
    "玩家信息需要明确昵称、KU ID或经过验证的引用；在线0人不代表没有历史玩家。"
    "需要分析时，先给简短结论，再用“📋 已核实”“💡 分析与建议”分开事实和推断，"
    "这是QQ纯文本消息：使用短段、图标与项目符号，不使用Markdown粗体、反引号、标题或表格；"
    "不得删除失败、不确定或存档时间说明。"
    "只有本次读取过的事实才能写成当前结论；历史上下文不能冒充实时采样。"
    "操作能力与已授权内容以当前目录为准，不能从菜单举例推断已经开放了某项写操作。"
)
READ_TOOLS = frozenset({"dst_rooms", "dst_room_query", "dst_player_query", "dst_servers", "dst_player", "dst_world", "dst_read", "dst_find", "dst_result"})
DST_TOOLS = READ_TOOLS | {"dst_game_action", "dst_job", "dst_find_api", "dst_call_api", "dst_player_action", "dst_server_action", "dst_run"}


@dataclass
class DisplayReceipt:
    owner: object
    projection: tuple
    source: str
    requested: object
    records: list = field(default_factory=list)
    foreign_tool: bool = False
    tool_count: int = 0
    original_hash: str = ""
    applied_text: str = ""
    dst_used: bool = False
    request_bound: bool = True
    superseded: bool = False


def requested_intent(text, aliases):
    # Explicit AI prefix still allows analysis requests through; only a full,
    # known read intent may use the exact card at the final response boundary.
    intent = read_request_intent(text, aliases)
    return (
        intent
        if intent
        and not intent.all_rooms
        and intent.kind in READ_KINDS - {"rooms", "help", "backups"}
        else None
    )


class ModelPresentation:
    def __init__(self):
        self.owner = object()
        self.active = OrderedDict()

    def get(self, event):
        value = getattr(event, "_dst_display_receipt", None)
        return value if isinstance(value, DisplayReceipt) and value.owner is self.owner else None

    def begin(self, event, actor, source, text, aliases):
        existing = self.get(event)
        if existing:
            return
        kind = requested_intent(text, aliases)
        event._dst_display_receipt = DisplayReceipt(
            self.owner,
            actor.projection,
            source,
            kind,
            dst_used=bool(kind or parse(text, aliases)),
        )
        message = str(getattr(getattr(event, "message_obj", None), "message_id", ""))
        try:
            reference = weakref.ref(event)
        except TypeError:
            return  # Minimal legacy adapters still use the response boundary.
        self.active[(actor.key, message)] = reference
        while len(self.active) > 256:
            self.active.popitem(last=False)

    def stop(self, event):
        """Stop this model event, never its already submitted game operation."""
        value = self.get(event)
        if value:
            value.superseded = True
        event._dst_suppressed_model = self.owner
        setter = getattr(event, "set_extra", None)
        if callable(setter):
            # AstrBot's existing runner watcher cancels an in-flight provider
            # request and exits the tool loop. No session-wide cancellation.
            setter("agent_stop_requested", True)
        stopper = getattr(event, "stop_event", None)
        if callable(stopper):
            stopper()

    def supersede(self, key, message):
        for identity, reference in list(self.active.items()):
            event = reference()
            value = self.get(event) if event is not None else None
            if value is None:
                self.active.pop(identity, None)
            elif identity[0] == key and identity[1] != message and value.request_bound:
                self.stop(event)
                self.active.pop(identity, None)

    def obsolete(self, event, state):
        if getattr(event, "_dst_suppressed_model", None) is self.owner:
            return True
        value = self.get(event)
        if not value or not value.request_bound:
            return False
        message = str(getattr(getattr(event, "message_obj", None), "message_id", ""))
        return value.superseded or not state.current_request(value.projection[0], message)

    def finish(self, event):
        value = self.get(event)
        if value:
            message = str(getattr(getattr(event, "message_obj", None), "message_id", ""))
            identity = (value.projection[0], message)
            reference = self.active.get(identity)
            if reference and reference() is event:
                self.active.pop(identity, None)

    def close(self):
        for reference in list(self.active.values()):
            event = reference()
            value = self.get(event) if event is not None else None
            if value and value.request_bound:
                self.stop(event)
        self.active.clear()

    def tool(self, event, name):
        value = self.get(event)
        if value:
            if name in DST_TOOLS:
                value.dst_used = True
                value.request_bound = True
            elif not value.dst_used:
                # Before any tool is chosen, the model is still handling the
                # addressed request. A foreign tool explicitly delegates the
                # topic elsewhere; formatting still depends only on dst_used.
                value.request_bound = False
            value.tool_count += 1
            if name not in READ_TOOLS or value.tool_count > 8:
                value.foreign_tool = True

    def remember(self, event, actor, source, kind, result, room=""):
        value = self.get(event)
        if not value or actor.projection != value.projection or source != value.source:
            return
        if len(value.records) < 2 and isinstance(result.text, str) and 0 < len(result.text) <= 3500:
            value.records.append(
                (
                    kind,
                    result.text,
                    getattr(result, "target", None),
                    room,
                    getattr(result, "outcome", "matched"),
                )
            )
        else:
            value.foreign_tool = True

    def final(self, event, actor, source):
        value = self.get(event)
        if not value or not value.requested or value.foreign_tool or len(value.records) != 1:
            return None
        kind, text, target, room, outcome = value.records[0]
        intent = value.requested
        if kind != intent.kind or intent.room_name:
            return None
        if intent.room and (target.room if target else room) != intent.room:
            return None
        if (
            outcome == "matched"
            and intent.player
            and intent.player not in {"我", "自己", "他", "他的"}
        ):
            identity = (
                target.player
                if target and intent.player.startswith("KU_")
                else target.name
                if target
                else ""
            )
            if intent.player != identity:
                return None
        if not actor or actor.projection != value.projection or source != value.source:
            return "会话权限或数据源已变化，本次查询结果不再展示；请重新查询。"
        return text

    def formats(self, event, actor, source):
        value = self.get(event)
        return bool(
            value
            and value.dst_used
            and actor
            and actor.projection == value.projection
            and source == value.source
        )


def content_hash(value):
    return hashlib.sha256(value.encode()).hexdigest()
