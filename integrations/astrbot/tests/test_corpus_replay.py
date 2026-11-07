"""Automated L1/L2 corpus replay against the real Go bot catalog snapshot."""

import json
import sys
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace as NS

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from test_companion import actor
from test_registry_discovery import SnapshotGateway, load_snapshot

from astrbot_plugin_dst_companion.capabilities import Directory
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.state import State, Target
from astrbot_plugin_dst_companion.task_tools import TaskTools

CORPUS_DIR = (
    Path(__file__).resolve().parents[3]
    / "internal"
    / "intent"
    / "testdata"
    / "corpus"
)


def load_jsonl(path: Path):
    rows = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


class CorpusReplayTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.snapshot = load_snapshot()
        self.all_views = frozenset(
            {"status", "players", "profile", "inventory", "network", "mods", "performance", "backups", "world"}
        )

    def make_actor(self, role_kind: str):
        if role_kind == "admin":
            return replace(
                actor(role="game-manage"),
                visibility=self.all_views,
                write_allowlist=frozenset({"*"}),
                workbench_mode="all",
            )
        return replace(
            actor(role="game-read"),
            visibility=self.all_views,
            write_allowlist=frozenset(),
            workbench_mode="off",
        )

    async def test_single_turn_corpus_replays_through_task_discovery(self):
        gw = SnapshotGateway(self.snapshot)
        directory = Directory(gw)
        tools = TaskTools(NS(directory=directory))
        admin = self.make_actor("admin")

        cases = load_jsonl(CORPUS_DIR / "single_turn.jsonl")
        self.assertGreaterEqual(len(cases), 30)

        for case in cases:
            cid = case["id"]
            query = case["query"]
            # Strip leading @bot for TaskTools.find (which receives model query text)
            clean_q = query.removeprefix("@bot").strip()
            found = json.loads(await tools.find(admin, clean_q))
            items = found.get("items", [])
            if "forbid_capability" in case:
                forbid = case["forbid_capability"]
                for item in items:
                    self.assertNotEqual(item["id"], forbid, f"[{cid}] {query} leaked to {forbid}")
                continue

            self.assertTrue(items, f"[{cid}] {query} returned no items")
            top = items[0]
            self.assertEqual(top["id"], case["want_capability"], f"[{cid}] {query} -> {items}")
            if "want_tier" in case:
                self.assertEqual(top.get("tier"), case["want_tier"], f"[{cid}] {query}")
            if "want_confirm" in case:
                self.assertEqual(top.get("confirm"), case["want_confirm"], f"[{cid}] {query}")

    async def test_multi_turn_dialogues_corpus_replays_end_to_end(self):
        dialogues = load_jsonl(CORPUS_DIR / "dialogues.jsonl")
        self.assertGreaterEqual(len(dialogues), 8)

        for dlg in dialogues:
            with self.subTest(dialogue=dlg["id"]):
                gw = SnapshotGateway(self.snapshot)
                state = State()
                directory = Directory(gw)
                records = {}

                async def load():
                    return dict(records)

                async def save(value):
                    records.clear()
                    records.update(value)

                actions = Actions(gw, load, save, directory=directory)
                try:
                    engine = Engine(gw, state, default_room="r1", actions=actions, directory=directory)
                    who = self.make_actor(dlg.get("role", "admin"))
                    for qid, qinfo in (dlg.get("setup_quotes") or {}).items():
                        state.associate(
                            who.scope,
                            qid,
                            Target(qinfo["room"], qinfo.get("player", ""), qinfo.get("name", "")),
                        )
                    for idx, turn in enumerate(dlg["turns"], start=1):
                        turn_actor = replace(
                            who,
                            event=NS(
                                message_obj=NS(message_id=f"{dlg['id']}-m{idx}"),
                                unified_msg_origin="aiocqhttp:GroupMessage:10001",
                            ),
                        )
                        reply = await engine.handle(
                            turn_actor,
                            turn["text"],
                            woken=True,
                            quote=turn.get("quote", ""),
                        )
                        self.assertEqual(
                            reply.outcome,
                            turn["want_outcome"],
                            f"[{dlg['id']} turn {idx}] {turn['text']} -> {reply.outcome}: {reply.text}",
                        )
                        for snippet in turn.get("want_in", []):
                            self.assertIn(snippet, reply.text, f"[{dlg['id']} turn {idx}]")
                        for forbidden in turn.get("want_not_in", []):
                            self.assertNotIn(forbidden, reply.text, f"[{dlg['id']} turn {idx}]")
                        if "want_exec" in turn:
                            want_cap, want_params = turn["want_exec"]
                            self.assertTrue(gw.executions, f"[{dlg['id']} turn {idx}] no execution")
                            self.assertEqual(
                                gw.executions[-1],
                                (want_cap, want_params),
                                f"[{dlg['id']} turn {idx}]",
                            )
                finally:
                    await actions.close()


if __name__ == "__main__":
    unittest.main()
