"""上下文策略：全量保留、滑窗裁剪、工具配对完整性与 CLI 单参数切换。"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent.context import (
    DEFAULT_CONTEXT_STRATEGY,
    DEFAULT_WINDOW_TURNS,
    STRATEGIES,
    plan_context,
)
from agent.llm import FakeClient, Message, ToolCall, Usage
from agent.loop import RunCheckpoint, RunResult, run
from agent.tools import build_registry
from agent.trace import Tracer


def _call(call_id: str, name: str = "search_files", pattern: str = "TODO") -> Message:
    """构造一条带工具调用的 assistant 消息。"""
    return Message(
        role="assistant",
        content=None,
        tool_calls=[ToolCall(id=call_id, name=name, arguments=json.dumps({"pattern": pattern}))],
    )


def _tool(call_id: str, content: str = "结果") -> Message:
    return Message(role="tool", tool_call_id=call_id, name="search_files", content=content)


def _conversation(turns: int) -> list[Message]:
    """system + user 锚点，后接若干轮 assistant/tool 交互。"""
    messages = [
        Message(role="system", content="系统提示"),
        Message(role="user", content="任务"),
    ]
    for index in range(turns):
        call_id = f"c{index}"
        messages.append(_call(call_id, pattern=f"P{index}"))
        messages.append(_tool(call_id, content=f"第 {index} 轮结果"))
    return messages


def test_full_keeps_every_message() -> None:
    """full 策略必须原样保留全部消息，不产生裁剪。"""
    messages = _conversation(5)
    plan = plan_context(messages, "full")

    assert plan.strategy == "full"
    assert plan.messages == messages
    assert plan.messages is not messages, "应返回新列表，避免调用方误改会话"
    assert (plan.total_messages, plan.used_messages, plan.dropped_messages) == (12, 12, 0)


def test_window_keeps_anchor_and_latest_turns() -> None:
    """window 策略保留 system/user 锚点与最近 N 轮，整轮丢弃更早轮次。"""
    messages = _conversation(4)
    plan = plan_context(messages, "window", window_turns=2)

    assert plan.strategy == "window"
    assert plan.total_messages == 10
    assert plan.used_messages == 6
    assert plan.dropped_messages == 4
    assert [m.role for m in plan.messages] == [
        "system",
        "user",
        "assistant",
        "tool",
        "assistant",
        "tool",
    ]
    kept_ids = {
        call.id for m in plan.messages if m.role == "assistant" for call in m.tool_calls
    }
    assert kept_ids == {"c2", "c3"}


def test_window_larger_than_history_keeps_all() -> None:
    """窗口大于实际轮数时不裁剪，但仍标记为 window 策略。"""
    messages = _conversation(2)
    plan = plan_context(messages, "window", window_turns=10)

    assert plan.messages == messages
    assert plan.dropped_messages == 0
    assert plan.window_turns == 10


def test_window_never_leaves_orphan_tool_messages() -> None:
    """每条保留的 tool 消息都必须能对应到同批保留的 assistant 工具调用。"""
    messages = _conversation(3)
    plan = plan_context(messages, "window", window_turns=1)

    call_ids = {
        call.id for m in plan.messages if m.role == "assistant" for call in m.tool_calls
    }
    tool_ids = {m.tool_call_id for m in plan.messages if m.role == "tool"}
    assert tool_ids <= call_ids
    assert [m.role for m in plan.messages[:2]] == ["system", "user"]


def test_window_drops_leading_orphan_tool_message() -> None:
    """前缀后第一条不是 assistant 的孤立消息不能进入请求。"""
    messages = [
        Message(role="system", content="系统提示"),
        Message(role="user", content="任务"),
        Message(role="tool", tool_call_id="ghost", content="孤儿结果"),
        _call("c1"),
        _tool("c1"),
    ]
    plan = plan_context(messages, "window", window_turns=1)

    assert [m.role for m in plan.messages] == ["system", "user", "assistant", "tool"]
    assert plan.dropped_messages == 1


def test_window_keeps_all_calls_of_one_turn() -> None:
    """一轮包含多个工具调用时，整轮保留，不能只留其中一条结果。"""
    messages = [
        Message(role="system", content="系统提示"),
        Message(role="user", content="任务"),
        Message(
            role="assistant",
            content=None,
            tool_calls=[
                ToolCall(id="a", name="read_file", arguments='{"path": "a.py"}'),
                ToolCall(id="b", name="read_file", arguments='{"path": "b.py"}'),
            ],
        ),
        Message(role="tool", tool_call_id="a", content="A"),
        Message(role="tool", tool_call_id="b", content="B"),
    ]
    plan = plan_context(messages, "window", window_turns=1)

    assert [m.tool_call_id for m in plan.messages if m.role == "tool"] == ["a", "b"]


def test_context_plan_serializes_for_trace() -> None:
    """trace 字段要能直接比较两种策略的保留/丢弃条数。"""
    plan = plan_context(_conversation(3), "window", window_turns=1)
    trace = plan.to_trace()

    keys = ("strategy", "total", "used", "dropped", "window_turns")
    assert {key: trace[key] for key in keys} == {
        "strategy": "window",
        "total": 8,
        "used": 4,
        "dropped": 4,
        "window_turns": 1,
    }
    assert [item["index"] for item in trace["dropped_detail"]] == [0, 1]


@pytest.mark.parametrize(("strategy", "window"), [("window", 0), ("bogus", 4)])
def test_invalid_context_settings_raise(strategy: str, window: int) -> None:
    """非法策略或非正窗口必须显式报错，而不是静默退化。"""
    with pytest.raises(ValueError):
        plan_context(_conversation(2), strategy, window)


class _CapturingClient(FakeClient):
    """记录每次请求实际收到的消息，用于验证 run() 的接入。"""

    def __init__(self, script: list[list[ToolCall]]) -> None:
        super().__init__(script=script)
        self.seen: list[list[Message]] = []

    def complete(self, messages, tools=None, max_tokens=None):
        self.seen.append(list(messages))
        return super().complete(messages, tools, max_tokens)


def _script(steps: int) -> list[list[ToolCall]]:
    calls = [
        [ToolCall(f"c{index}", "search_files", json.dumps({"pattern": f"P{index}"}))]
        for index in range(steps)
    ]
    calls.append([ToolCall("fin", "finish", '{"summary": "完成"}')])
    return calls


def test_run_applies_window_and_records_context(tmp_path: Path) -> None:
    """run 只在请求侧裁剪；trace 记录每步保留条数，断点仍保存全量。"""
    client = _CapturingClient(_script(4))
    tracer = Tracer(path=tmp_path / "traces" / "window.jsonl")
    result = run(
        "统计",
        client,
        build_registry(tmp_path),
        tracer,
        max_steps=8,
        context_strategy="window",
        context_window=2,
    )
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert [len(messages) for messages in client.seen] == [2, 4, 6, 6, 6]
    assert all(messages[0].role == "system" for messages in client.seen)
    assert all(messages[1].role == "user" for messages in client.seen)

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    contexts = [event["context"] for event in events if event["kind"] == "llm"]
    assert [item["used"] for item in contexts] == [2, 4, 6, 6, 6]
    assert all(item["strategy"] == "window" for item in contexts)
    last = contexts[-1]
    assert {key: last[key] for key in ("strategy", "total", "used", "dropped", "window_turns")} == {
        "strategy": "window",
        "total": 10,
        "used": 6,
        "dropped": 4,
        "window_turns": 2,
    }
    assert [item["index"] for item in last["dropped_detail"]] == [0, 1]

    state = json.loads((tmp_path / "traces" / "window.state.json").read_text(encoding="utf-8"))
    assert len(state["messages"]) == 11, "断点必须保留全量对话，不能只存滑窗"
    assert state["context_strategy"] == "window"
    assert state["context_window"] == 2
    checkpoint = RunCheckpoint.load(tmp_path / "traces" / "window.state.json")
    assert (checkpoint.context_strategy, checkpoint.context_window) == ("window", 2)


def test_run_full_sends_growing_history(tmp_path: Path) -> None:
    """同一条脚本换 full 策略时请求消息持续增长，证明只改一个参数即可对照。"""
    client = _CapturingClient(_script(4))
    tracer = Tracer(path=tmp_path / "traces" / "full.jsonl")
    result = run(
        "统计",
        client,
        build_registry(tmp_path),
        tracer,
        max_steps=8,
        context_strategy="full",
    )
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert [len(messages) for messages in client.seen] == [2, 4, 6, 8, 10]
    assert set(STRATEGIES) == {"full", "window", "summary", "retrieval"}
    assert DEFAULT_WINDOW_TURNS >= 1


def test_run_inherits_context_settings_from_checkpoint(tmp_path: Path) -> None:
    """直接调用 run 且未传策略时，从断点读取原策略，不能悄悄回退到 full。"""
    checkpoint = RunCheckpoint(
        run_id="inherit",
        trace_path=str(tmp_path / "inherit.jsonl"),
        task="任务",
        messages=_conversation(3),
        step=1,
        next_step=1,
        pending_calls=[],
        next_call_index=0,
        usage=Usage(),
        total_cost_yuan=0.0,
        context_strategy="window",
        context_window=1,
    )
    client = _CapturingClient([[ToolCall("fin", "finish", '{"summary": "完成"}')]])
    tracer = Tracer(path=tmp_path / "traces" / "inherit.jsonl")

    result = run("任务", client, build_registry(tmp_path), tracer, checkpoint=checkpoint)
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert len(client.seen[0]) == 4, "锚点 2 条 + 最近 1 轮 2 条"
    assert [m.tool_call_id for m in client.seen[0] if m.role == "tool"] == ["c2"]


def test_run_default_strategy_comes_from_config(tmp_path: Path) -> None:
    """不传策略时 run 使用集中配置的默认值，而不是散落在循环里的字面量。"""
    client = _CapturingClient([[ToolCall("fin", "finish", '{"summary": "完成"}')]])
    tracer = Tracer(path=tmp_path / "traces" / "default.jsonl")
    result = run("任务", client, build_registry(tmp_path), tracer, max_steps=2)
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    contexts = [event["context"] for event in events if event["kind"] == "llm"]
    assert contexts[0]["strategy"] == DEFAULT_CONTEXT_STRATEGY


def test_cli_exposes_single_strategy_switch(monkeypatch, tmp_path: Path, capsys) -> None:
    """CLI 只用一个 --context-strategy 切换策略，并把窗口参数传进 run。"""
    import agent.trace as trace_module
    from agent.cli.main import main as cli_main

    captured: dict = {}

    def fake_run(task, client, registry, tracer, **kwargs):
        captured.update(kwargs, task=task)
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
            "window",
            "--context-window",
            "3",
        ]
    )
    out = capsys.readouterr().out

    assert code == 0
    assert captured["context_strategy"] == "window"
    assert captured["context_window"] == 3
    assert captured["closed"]["stop_reason"] == "finished"
    assert "[context] window（最近 3 轮）" in out


def test_cli_rejects_non_positive_window() -> None:
    """窗口为 0 时在装配前就拒绝，避免请求期才报错。"""
    from agent.cli.main import main as cli_main

    with pytest.raises(SystemExit) as excinfo:
        cli_main(["--task", "x", "--provider", "fake", "--context-window", "0"])
    assert excinfo.value.code == 2


def test_resume_inherits_context_settings(
    monkeypatch, tmp_path: Path, capsys
) -> None:
    """恢复时默认沿用断点里的策略；显式传参可以覆盖。"""
    import agent.trace as trace_module
    from agent.cli.main import main as cli_main

    trace = tmp_path / "resume.jsonl"
    trace.write_text(
        json.dumps({"kind": "run_start", "run_id": "resume", "task": "任务"}) + "\n",
        encoding="utf-8",
    )
    checkpoint = RunCheckpoint(
        run_id="resume",
        trace_path=str(trace),
        task="任务",
        messages=[
            Message(role="system", content="系统提示"),
            Message(role="user", content="任务"),
        ],
        step=1,
        next_step=1,
        pending_calls=[],
        next_call_index=0,
        usage=Usage(),
        total_cost_yuan=0.0,
        context_strategy="window",
        context_window=2,
    )
    checkpoint.save(trace.with_suffix(".state.json"))

    captured: dict = {}

    def fake_run(task, client, registry, tracer, **kwargs):
        captured.clear()
        captured.update(kwargs)
        return RunResult(
            answer="ok",
            steps=1,
            stop_reason="finished",
            trace_path=str(trace),
        )

    class StubTracer:
        def __init__(self, *args, **kwargs):
            self.path = trace

        def close(self, **kwargs):
            pass

        def abort(self, **kwargs):
            pass

    monkeypatch.setattr(trace_module, "Tracer", StubTracer)
    monkeypatch.setattr("agent.loop.run", fake_run)

    assert (
        cli_main(["--resume", str(trace), "--provider", "fake", "--root", str(tmp_path), "--quiet"])
        == 0
    )
    assert (captured["context_strategy"], captured["context_window"]) == ("window", 2)

    assert (
        cli_main(
            [
                "--resume",
                str(trace),
                "--provider",
                "fake",
                "--root",
                str(tmp_path),
                "--quiet",
                "--context-strategy",
                "full",
            ]
        )
        == 0
    )
    assert captured["context_strategy"] == "full"
    capsys.readouterr()
