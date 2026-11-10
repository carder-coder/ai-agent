"""循环骨架：用离线替身验证 步数上限、finish 收尾、trace 两类事件。"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent.cli.main import main
from agent.llm import FakeClient, LLMResponse, Message, ProviderTimeout, ToolCall
from agent.loop import RunCheckpoint, run
from agent.tools import ToolRegistry, build_registry
from agent.trace import Tracer


def test_loop_finishes(tmp_path: Path) -> None:
    """验证工具调用后执行 finish 时循环正常结束并记录完整 trace。"""
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
    """验证连续重复调用同一工具会触发 loop_guard 并停止空转。"""
    script = [
        [ToolCall(f"c{i}", "search_files", '{"pattern": "TODO", "glob": "*.py"}')]
        for i in range(5)
    ]
    tracer = Tracer(path=tmp_path / "traces" / "t2.jsonl")
    result = run("死循环", FakeClient(script=script), build_registry(tmp_path), tracer, max_steps=3)
    tracer.close(stop_reason=result.stop_reason)
    assert result.stop_reason == "no_new_info"
    assert result.steps == 3
    loop_guard = next(
        json.loads(line)
        for line in tracer.path.read_text(encoding="utf-8").splitlines()
        if json.loads(line)["kind"] == "loop_guard"
    )
    assert loop_guard["repeats"] == 3, "loop_guard 必须记录实际重复次数"


def test_loop_records_provider_error(tmp_path: Path) -> None:
    """验证上游超时会设置 provider_error 停止原因并写入 trace。"""
    class TimeoutClient(FakeClient):
        def complete(self, messages, tools=None, max_tokens=None):
            raise ProviderTimeout("上游超时：测试")

    tracer = Tracer(path=tmp_path / "traces" / "provider-error.jsonl")
    result = run("测试上游故障", TimeoutClient(), build_registry(tmp_path), tracer)
    tracer.close(stop_reason=result.stop_reason)

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    assert result.stop_reason == "provider_error", "上游错误必须设置正确 stop_reason"
    assert result.answer.startswith("上游不可用："), "用户可见答案需说明上游不可用"
    assert any(event["kind"] == "provider_error" for event in events), (
        "trace 必须记录 provider_error 事件"
    )


def test_tool_failure_is_returned_to_model_and_loop_continues(tmp_path: Path) -> None:
    """验证工具异常以 ERROR observation 回传模型后循环仍可继续完成。"""
    from pydantic import BaseModel

    class Args(BaseModel):
        value: str

    class CapturingClient(FakeClient):
        def __init__(self, script):
            super().__init__(script=script)
            self.received_messages = []

        def complete(self, messages, tools=None, max_tokens=None):
            self.received_messages.append(list(messages))
            return super().complete(messages, tools, max_tokens)

    registry = ToolRegistry(root=tmp_path)

    def fail(_args: Args) -> str:
        raise RuntimeError("预期测试异常")

    registry.register("fail", "总是失败的测试工具", Args)(fail)
    client = CapturingClient(
        script=[
            [ToolCall("fail-1", "fail", '{"value": "test"}')],
            [ToolCall("finish-1", "finish", '{"summary": "已处理失败"}')],
        ]
    )
    tracer = Tracer(path=tmp_path / "traces" / "tool-error.jsonl")
    result = run("测试工具失败回填", client, registry, tracer)
    tracer.close(stop_reason=result.stop_reason)

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    tool_event = next(event for event in events if event["kind"] == "tool")
    feedback = next(message for message in client.received_messages[1] if message.role == "tool")
    assert tool_event["ok"] is False, "失败工具的 trace 必须标记 ok=False"
    assert feedback.content.startswith("ERROR: "), "失败 observation 必须带 ERROR 前缀"
    assert "预期测试异常" in feedback.content
    assert result.stop_reason == "finished", "工具失败后循环仍应继续并能完成"
    assert result.steps == 2


def test_loop_finishes_with_plain_text_response(tmp_path: Path) -> None:
    """验证模型直接返回文本且不请求工具时以 no_tool_call 结束。"""
    class TextClient(FakeClient):
        def complete(self, messages, tools=None, max_tokens=None):
            return LLMResponse(
                message=Message(role="assistant", content="这是直接回答"),
            )

    tracer = Tracer(path=tmp_path / "traces" / "plain-text.jsonl")
    result = run("直接回答", TextClient(), build_registry(tmp_path), tracer)
    tracer.close(stop_reason=result.stop_reason)

    assert result.stop_reason == "no_tool_call", "无工具调用的响应应走 no_tool_call 分支"
    assert result.answer == "这是直接回答"


@pytest.mark.parametrize("resume_with_state_file", [False, True])
def test_loop_resumes_pending_tool_calls_without_replaying_completed_ones(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    resume_with_state_file: bool,
) -> None:
    """验证从 trace 或 state 恢复时继续待处理调用，且不重复已完成调用。"""
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
