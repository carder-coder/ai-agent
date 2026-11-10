"""BM25 检索裁剪：分词、打分、预算选择、最近轮保留与 trace。"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent.context import (
    RETRIEVAL_BUDGET_CHARS,
    RETRIEVAL_KEEP_RECENT,
    bm25_scores,
    plan_retrieval_context,
    tokenize,
)
from agent.context._layout import anchor_end, estimate_context_chars, split_turns
from agent.llm import FakeClient, Message, ToolCall, Usage
from agent.loop import RunCheckpoint, RunResult, run
from agent.tools import build_registry
from agent.trace import Tracer


def _call(call_id: str, name: str, args: str) -> Message:
    return Message(
        role="assistant",
        content=None,
        tool_calls=[ToolCall(id=call_id, name=name, arguments=args)],
    )


def _tool(call_id: str, name: str, content: str) -> Message:
    return Message(role="tool", tool_call_id=call_id, name=name, content=content)


def _task_conversation() -> list[Message]:
    """三旧轮 + 一最近轮：Python 轮相关、计划轮无关、pyproject 轮相关。"""
    return [
        Message(role="system", content="你是仓库分析助手"),
        Message(role="user", content="请总结 README 中的 Python 版本与依赖管理"),
        _call("c0", "search_files", '{"pattern": "Python"}'),
        _tool("c0", "search_files", "README.md: Python 3.13，使用 uv 管理依赖"),
        _call("c1", "read_file", '{"path": "docs/plan/week-01.md"}'),
        _tool("c1", "read_file", "第一周计划：复盘与预习，与当前任务无关"),
        _call("c2", "read_file", '{"path": "pyproject.toml"}'),
        _tool("c2", "read_file", "requires-python = \">=3.13\"，依赖 httpx 与 pydantic"),
        _call("c3", "search_files", '{"pattern": "TODO"}'),
        _tool("c3", "search_files", "没有命中"),
    ]


def test_tokenize_splits_ascii_and_cjk_bigrams() -> None:
    tokens = tokenize("Python 3.13 依赖管理")

    assert "python" in tokens
    assert "3" in tokens and "13" in tokens
    assert "依赖" in tokens and "管理" in tokens
    assert "赖管" in tokens, "中文按相邻二元组切分"


def test_bm25_ranks_relevant_turns_above_noise() -> None:
    documents = [
        "Python 3.13 使用 uv 管理依赖",
        "第一周计划复盘与预习",
        "requires-python >=3.13",
    ]

    scores = bm25_scores(documents, "Python 版本 依赖")

    assert scores[0] > scores[1]
    assert scores[2] > scores[1]
    assert scores[1] == 0.0


def test_retrieval_selects_relevant_turns_within_budget() -> None:
    messages = _task_conversation()
    anchor = anchor_end(messages)
    turns = split_turns(messages, anchor)
    base = estimate_context_chars(messages[:anchor]) + estimate_context_chars(turns[-1])
    budget = base + estimate_context_chars(turns[0]) + estimate_context_chars(turns[2])

    plan = plan_retrieval_context(messages, budget_chars=budget, keep_recent=1)

    assert plan.strategy == "retrieval"
    assert plan.retrieval is not None
    assert plan.retrieval["selected_indexes"] == [0, 2]
    assert plan.retrieval["dropped"] == 1
    assert plan.dropped_messages == 2
    assert [m.tool_call_id for m in plan.messages if m.role == "tool"] == ["c0", "c2", "c3"]
    assert plan.dropped_detail is not None
    assert plan.dropped_detail[0]["index"] == 1
    assert "第一周计划" in plan.dropped_detail[0]["preview"]
    assert "score" in plan.dropped_detail[0]


def test_retrieval_always_keeps_most_recent_turn() -> None:
    messages = _task_conversation()

    plan = plan_retrieval_context(messages, budget_chars=1, keep_recent=1)

    assert plan.retrieval is not None
    assert plan.retrieval["selected"] == 0
    assert plan.retrieval["over_budget"] is True
    assert [m.tool_call_id for m in plan.messages if m.role == "tool"] == ["c3"]
    assert [m.role for m in plan.messages[:2]] == ["system", "user"]


def test_retrieval_without_old_turn_keeps_full() -> None:
    messages = [
        Message(role="system", content="系统提示"),
        Message(role="user", content="任务"),
        _call("c1", "read_file", '{"path": "README.md"}'),
        _tool("c1", "read_file", "内容"),
    ]

    plan = plan_retrieval_context(messages, budget_chars=10)

    assert plan.messages == messages
    assert plan.retrieval is not None
    assert plan.retrieval["reason"] == "no_old_turns"


def test_retrieval_rejects_bad_settings() -> None:
    messages = _task_conversation()
    with pytest.raises(ValueError):
        plan_retrieval_context(messages, budget_chars=0)
    with pytest.raises(ValueError):
        plan_retrieval_context(messages, keep_recent=0)


def test_retrieval_params_have_central_defaults() -> None:
    assert RETRIEVAL_BUDGET_CHARS > 0
    assert RETRIEVAL_KEEP_RECENT >= 1


class _CapturingFake(FakeClient):
    def __init__(self, script: list[list[ToolCall]]) -> None:
        super().__init__(script=script)
        self.seen: list[list[Message]] = []

    def complete(self, messages, tools=None, max_tokens=None):
        self.seen.append(list(messages))
        return super().complete(messages, tools, max_tokens)


def test_run_uses_retrieval_and_records_trace(tmp_path: Path) -> None:
    """run 的 retrieval 分支要裁剪请求、保留配对，并把检索统计写进 trace。"""
    script = [
        [ToolCall(f"c{index}", "search_files", json.dumps({"pattern": f"P{index}"}))]
        for index in range(4)
    ]
    script.append([ToolCall("fin", "finish", '{"summary": "完成"}')])
    client = _CapturingFake(script)
    tracer = Tracer(path=tmp_path / "traces" / "retrieval.jsonl")
    result = run(
        "统计",
        client,
        build_registry(tmp_path),
        tracer,
        max_steps=8,
        context_strategy="retrieval",
        retrieval_budget=1,
    )
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert [len(messages) for messages in client.seen] == [2, 4, 4, 4, 4]

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    contexts = [event["context"] for event in events if event["kind"] == "llm"]
    retrieval = [context.get("retrieval") for context in contexts if context.get("retrieval")]
    assert retrieval, "至少从第三步起应有 retrieval 统计"
    assert retrieval[-1]["enabled"] is True
    assert retrieval[-1]["dropped"] > 0


def test_checkpoint_round_trips_retrieval_budget(tmp_path: Path) -> None:
    path = tmp_path / "checkpoint.state.json"
    checkpoint = RunCheckpoint(
        run_id="retrieval",
        trace_path=str(tmp_path / "trace.jsonl"),
        task="任务",
        messages=[
            Message(role="system", content="系统提示"),
            Message(role="user", content="任务"),
        ],
        step=1,
        next_step=1,
        pending_calls=[],
        next_call_index=0,
        context_strategy="retrieval",
        usage=Usage(),
        total_cost_yuan=0.0,
        retrieval_budget=777,
    )
    checkpoint.save(path)
    loaded = RunCheckpoint.load(path)

    assert loaded.retrieval_budget == 777


def test_cli_passes_retrieval_budget(monkeypatch, tmp_path: Path, capsys) -> None:
    import agent.trace as trace_module
    from agent.cli.main import main as cli_main

    captured: dict = {}

    def fake_run(task, client, registry, tracer, **kwargs):
        captured.update(kwargs)
        return RunResult(
            answer="ok",
            steps=1,
            stop_reason="finished",
            trace_path=str(tmp_path / "traces" / "stub.jsonl"),
        )

    class StubTracer:
        def __init__(self, *args, **kwargs):
            self.path = tmp_path / "traces" / "stub.jsonl"

        def close(self, **kwargs):
            captured["closed"] = kwargs

        def abort(self, **kwargs):
            raise AssertionError("成功路径不应触发 abort")

    monkeypatch.setattr(trace_module, "Tracer", StubTracer)
    monkeypatch.setattr("agent.loop.run", fake_run)

    code = cli_main(
        [
            "--task",
            "冒烟",
            "--provider",
            "fake",
            "--root",
            str(tmp_path),
            "--context-strategy",
            "retrieval",
            "--retrieval-budget",
            "777",
        ]
    )
    out = capsys.readouterr().out

    assert code == 0
    assert captured["retrieval_budget"] == 777
    assert "[context] retrieval（预算 777 字符）" in out
