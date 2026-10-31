"""Admin-only staged template editing, synthetic previews and atomic group saves."""

import copy
import time

from .configuration import fingerprint
from .gateway import QueryError
from .message_templates import (
    DEFINITIONS,
    render_text,
    revision,
    rows_from,
    sample_values,
    stored_templates,
    template_id,
    validate_templates,
)
from .sessions import group_origin

HELP = (
    "🎨 消息模板（本群 > 全局 > 内置；管理员专用）\n"
    "/dst 模板 列表\n/dst 模板 查看 房间概况\n/dst 模板 编辑 聊天播报 正文\n"
    "/dst 模板 预览 聊天播报\n/dst 模板 保存\n/dst 模板 恢复 聊天播报\n"
    "/dst 模板 取消\n全局设置：/dst 模板 全局 编辑 聊天播报 正文\n"
    "其他全局操作也在“模板”后加“全局”。编辑/恢复先进入草稿，5 分钟内保存；"
    "恢复表示清除本层覆盖、继承上层。双花括号 {{ }} 显示字面花括号。"
)


class TemplateEditor:
    def __init__(self, plugin, clock=time.monotonic):
        self.plugin, self.clock, self.drafts = plugin, clock, {}

    async def handle(self, event, argument, reply):
        p = self.plugin
        # Setup has already claimed the command and checked native admin.
        session = group_origin(str(event.unified_msg_origin))
        words = argument.split(maxsplit=1)
        global_scope = bool(words and words[0] == "全局")
        if global_scope:
            argument = words[1] if len(words) > 1 else ""
        words = argument.split(maxsplit=1)
        operation = words[0] if words else "列表"
        rest = words[1] if len(words) > 1 else ""
        key = (session, str(event.get_sender_id()), global_scope)
        self.drafts = {k: v for k, v in self.drafts.items() if v["until"] > self.clock()}
        draft = self.drafts.get(key)
        try:
            await p.ensure_enrollment()
        except Exception:
            await reply("模板读取暂不可用，设置未更改。")
            return
        if operation in {"帮助", "列表"}:
            lines = [HELP, "\n可用模板："]
            for kind, definition in DEFINITIONS.items():
                _, source = p.message_templates.selected(session, kind, global_scope=global_scope)
                lines.append(definition.label + " · " + kind + " · " + source)
            await reply("\n".join(lines))
            return
        if operation == "取消":
            self.drafts.pop(key, None)
            await reply("已取消本次模板草稿；已保存配置未更改。")
            return
        try:
            record = p.enrollment.records.get(session, {})
            if (
                operation in {"编辑", "恢复", "保存"}
                and not global_scope
                and (event.is_private_chat() or not record.get("enabled"))
            ):
                await reply("请在已启用的群内编辑本群模板；私聊可用 /dst 模板 全局 编辑。")
                return
            if operation == "保存":
                if rest or not draft:
                    await reply("没有待保存草稿；先编辑或恢复，再预览并保存。")
                    return
                validate_templates(draft["rows"])
                if global_scope:
                    candidate = copy.deepcopy(dict(p.config))
                    candidate["message_templates"] = draft["rows"]
                    await p.config_editor.apply(
                        candidate, draft["expected"], ["__templates_global__"]
                    )
                    p.message_templates.refresh()
                else:
                    await p.enrollment.update_templates(session, draft["rows"], draft["expected"])
                current = self.drafts.get(key)
                if current is draft:
                    self.drafts.pop(key, None)
                elif current and current["expected"] == draft["expected"]:
                    # A new edit arrived while persistence was awaiting I/O.
                    # It contains this saved draft plus subsequent edits.
                    current["expected"] = (
                        fingerprint(dict(p.config)) if global_scope else revision(draft["rows"])
                    )
                await reply(
                    "✅ 模板已保存，后续新消息立即生效。已排队通知保留原样。"
                    + (
                        "保存期间的新编辑仍保留为草稿，请另行预览保存。"
                        if current is not draft and current
                        else ""
                    )
                )
                return
            args = rest.split(maxsplit=1)
            kind = template_id(args[0]) if args else None
            if not kind or operation not in {"查看", "预览", "编辑", "恢复"}:
                await reply(HELP)
                return
            if operation in {"查看", "预览", "恢复"} and len(args) > 1:
                await reply("此操作只需指定模板类型；设置未更改。")
                return
            if operation in {"编辑", "恢复"}:
                rows = draft["rows"] if draft else p.message_templates.rows(session, global_scope)
                values, _ = stored_templates(rows)
                if operation == "编辑":
                    if len(args) < 2:
                        await reply(
                            "请在类型后提供多行正文；例如 /dst 模板 编辑 聊天播报 💬 {room_name}\n{lines}"
                        )
                        return
                    values[kind] = args[1]
                else:
                    values.pop(kind, None)
                rows = rows_from(values)
                validate_templates(rows)
                if not draft and len(self.drafts) >= 64:
                    raise ValueError("编辑会话已达上限，请稍后再试")
                self.drafts[key] = {
                    "rows": rows,
                    "until": self.clock() + 300,
                    "expected": draft["expected"]
                    if draft
                    else (
                        fingerprint(dict(p.config))
                        if global_scope
                        else revision(p.message_templates.rows(session))
                    ),
                }
                await reply(
                    "已暂存模板草稿；可用 /dst 模板 "
                    + ("全局 " if global_scope else "")
                    + "预览 "
                    + kind
                    + " 查看，确认后用同范围“保存”生效。"
                )
                return
            source, origin = p.message_templates.selected(
                session,
                kind,
                draft=draft["rows"] if draft else None,
                global_scope=global_scope,
            )
            definition = DEFINITIONS[kind]
            header = "🎨 " + definition.label + " · " + ("草稿" if draft else origin)
            if operation == "查看":
                await reply(
                    header
                    + "\n可用变量："
                    + " ".join("{" + f + "}" for f in sorted(definition.fields))
                    + "\n\n"
                    + source
                )
            else:
                await reply(
                    header
                    + "\n【模拟预览 · 不是真实数据，未执行游戏操作】\n\n"
                    + render_text(kind, source, sample_values(kind))
                )
        except ValueError as error:
            await reply("模板未保存：" + str(error)[:200])
        except QueryError as error:
            await reply("模板未保存：" + error.code + "。配置可能已变化，请取消草稿后重新编辑。")
        except Exception:
            await reply("模板持久化未确认，未应用新模板；请查看当前模板后重试。")
