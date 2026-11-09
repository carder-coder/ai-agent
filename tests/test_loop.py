"""循环骨架：用离线替身验证 步数上限、finish 收尾、trace 两类事件。"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent.cli.main import main
from agent.llm import FakeClient, ToolCall
from agent.loop import RunCheckpoint, run
from agent.tools import build_registry
from agent.trace import Tracer


def test_loop_finishes(tmp_path: Path) -> None:
    (tmp_path / "a.py").write_text("# TODO one\n", encoding="utf-8")
    tracer = Tracer(path=tmp_path / "traces" / "t.jsonl")
    result = run("统计 TODO", FakeClient(), build_registry(tmp_path), tracer, max_steps=6)
    tracer.close(stop_reason=result.stop_reason)
    assert result.stop_reason == "finished"
    assert "离线冒烟完成" in result.answer
    assert result.steps == 2

    kinds = [
        json.loads(line)["kind"]
        for line in tracer.path.read_text(encoding="utf-8").splitlines()
    ]
    tool_events = [
        json.loads(line)
        for line in tracer.path.read_text(encoding="utf-8").splitlines()
        if json.loads(line)["kind"] == "tool"
    ]
    assert kinds.count("llm") == 2
    assert "tool" in kinds
    assert tool_events[0]["arguments"] == '{"pattern": "TODO|FIXME", "glob": "*.py"}'
    assert kinds[-1] == "run_end"


def test_loop_stops_on_max_steps(tmp_path: Path) -> None:
    script = [
        [ToolCall(f"c{i}", "search_files", '{"pattern": "TODO", "glob": "*.py"}')]
        for i in range(5)
    ]
    tracer = Tracer(path=tmp_path / "traces" / "t2.jsonl")
    result = run("死循环", FakeClient(script=script), build_registry(tmp_path), tracer, max_steps=3)
    tracer.close(stop_reason=result.stop_reason)
    assert result.stop_reason == "no_new_info"
    assert result.steps == 3


@pytest.mark.parametrize("resume_with_state_file", [False, True])
def test_loop_resumes_pending_tool_calls_without_replaying_completed_ones(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    resume_with_state_file: bool,
) -> None:
    scripts = [
        [ToolCall("c1", "search_files", '{"pattern": "TODO", "glob": "*.py"}')],
        [ToolCall("c2", "search_files", '{"pattern": "FIXME", "glob": "*.py"}')],
        [
            ToolCall("c3a", "search_files", '{"pattern": "MATCH", "glob": "*.py"}'),
            ToolCall("c3b", "search_files", '{"pattern": "OTHER", "glob": "*.py"}'),
        ],
    ]

    class InterruptingClient(FakeClient):
        def complete(self, messages, tools=None, max_tokens=None):
            if self.step == len(scripts):
                raise KeyboardInterrupt
            return super().complete(messages, tools, max_tokens)

    registry = build_registry(tmp_path)
    original_invoke = registry.invoke
    invoke_count = 0

    def interrupt_before_fourth_tool(name: str, raw_args: str):
        nonlocal invoke_count
        invoke_count += 1
        if invoke_count == 4:
            raise KeyboardInterrupt
        return original_invoke(name, raw_args)

    monkeypatch.setattr(registry, "invoke", interrupt_before_fourth_tool)
    trace_path = tmp_path / "traces" / "resume.jsonl"
    tracer = Tracer(path=trace_path)
    with pytest.raises(KeyboardInterrupt):
        run("统计标记", InterruptingClient(script=scripts), registry, tracer, max_steps=12)
    tracer.abort()

    state_path = trace_path.with_suffix(".state.json")
    checkpoint = RunCheckpoint.load(state_path)
    assert checkpoint.step == 3
    assert checkpoint.next_step == 3
    assert checkpoint.next_call_index == 1
    assert len(checkpoint.pending_calls) == 2
    assert sum(message.role == "tool" for message in checkpoint.messages) == 3

    class CapturingClient(FakeClient):
        instances = []

        def __init__(self):
            super().__init__(script=[[ToolCall("finish", "finish", '{"summary": "完成"}')]])
            self.received_messages = []
            self.instances.append(self)

        def complete(self, messages, tools=None, max_tokens=None):
            self.received_messages = list(messages)
            return super().complete(messages, tools, max_tokens)

    monkeypatch.setattr("agent.llm.FakeClient", CapturingClient)
    resume_path = state_path if resume_with_state_file else trace_path
    assert main(
        [
            "--resume",
            str(resume_path),
            "--provider",
            "fake",
            "--root",
            str(tmp_path),
            "--max-steps",
            "12",
            "--quiet",
        ]
    ) == 0
    client = CapturingClient.instances[-1]

    events = [json.loads(line) for line in trace_path.read_text(encoding="utf-8").splitlines()]
    tool_events = [event for event in events if event["kind"] == "tool"]
    resumed_start = [
        event for event in events if event["kind"] == "run_start" and event.get("resumed_from")
    ]
    run_end = next(event for event in reversed(events) if event["kind"] == "run_end")
    assert len(resumed_start) == 1
    assert run_end["steps"] == 4
    assert run_end["stop_reason"] == "finished"
    assert sum('"pattern": "MATCH"' in event["arguments"] for event in tool_events) == 1
    assert sum('"pattern": "OTHER"' in event["arguments"] for event in tool_events) == 1
    assert sum(message.role == "tool" for message in client.received_messages) == 4
    assert run_end["usage"]["total"] == 600
