"""摘要策略：阈值触发、固定提示版本、缓存、trace 计费与失败降级。"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent.context import (
    DEFAULT_SUMMARY_VERSION,
    SUMMARY_MAX_TOKENS,
    SUMMARY_PROMPTS,
    Summarizer,
    SummaryError,
    SummaryOutcome,
    estimate_context_chars,
    plan_summary_context,
)
from agent.llm import (
    FakeClient,
    LLMResponse,
    Message,
    ProviderError,
    ToolCall,
    Usage,
)
from agent.loop import RunCheckpoint, RunResult, run
from agent.tools import build_registry
from agent.trace import Tracer


def _call(call_id: str, pattern: str = "TODO") -> Message:
    return Message(
        role="assistant",
        content=None,
        tool_calls=[
            ToolCall(
                id=call_id,
                name="search_files",
                arguments=json.dumps({"pattern": pattern}),
            )
        ],
    )


def _tool(call_id: str, content: str = "结果") -> Message:
    return Message(role="tool", tool_call_id=call_id, name="search_files", content=content)


def _conversation(turns: int, size: int = 10) -> list[Message]:
    messages = [
        Message(role="system", content="系统提示"),
        Message(role="user", content="任务"),
    ]
    for index in range(turns):
        call_id = f"c{index}"
        messages.append(_call(call_id, pattern=f"P{index}"))
        messages.append(_tool(call_id, content="结" * size))
    return messages


def _outcome(text: str = "历史摘要正文", covered: int = 0) -> SummaryOutcome:
    return SummaryOutcome(text=text, version="v1", covered_messages=covered)


def test_below_threshold_behaves_like_full() -> None:
    """未超过字符阈值时不调用摘要，请求保留全部消息。"""
    messages = _conversation(3, size=100)
    calls: list[list[Message]] = []

    plan, outcome = plan_summary_context(
        messages,
        lambda dropped: calls.append(dropped) or _outcome(),
        keep_turns=2,
        trigger_chars=100_000,
    )

    assert outcome is None
    assert calls == []
    assert plan.messages == messages
    assert plan.summary is not None
    assert plan.summary["enabled"] is False
    assert plan.summary["reason"] == "below_threshold"


def test_over_threshold_summarizes_older_turns_once() -> None:
    """超阈值时压缩更早轮次，摘要插在 system 与 user 之间，最近轮保持完整。"""
    messages = _conversation(4, size=100)
    calls: list[list[Message]] = []

    plan, outcome = plan_summary_context(
        messages,
        lambda dropped: calls.append(dropped) or _outcome(covered=len(dropped)),
        keep_turns=2,
        trigger_chars=100,
        version="v1",
    )

    assert len(calls) == 1
    assert [m.tool_call_id for m in calls[0] if m.role == "tool"] == ["c0", "c1"]
    assert outcome is not None and outcome.version == "v1"

    assert [m.role for m in plan.messages[:4]] == ["system", "system", "user", "assistant"]
    summary_message = plan.messages[1]
    assert "历史摘要正文" in summary_message.content
    assert "v1" in summary_message.content
    assert [m.tool_call_id for m in plan.messages if m.role == "tool"] == ["c2", "c3"]

    assert plan.summary is not None
    assert plan.summary["enabled"] is True
    assert plan.summary["covered"] == 4
    assert plan.summary["keep_turns"] == 2
    assert (plan.total_messages, plan.used_messages, plan.dropped_messages) == (10, 7, 3)


def test_over_threshold_with_few_turns_still_summarizes() -> None:
    """只有两轮时也保留最近一轮、压缩更早一轮，避免阈值触发却永不裁剪。"""
    messages = _conversation(2, size=100)
    calls: list[list[Message]] = []

    plan, _ = plan_summary_context(
        messages,
        lambda dropped: calls.append(dropped) or _outcome(),
        keep_turns=4,
        trigger_chars=100,
    )

    assert len(calls) == 1
    assert plan.summary is not None and plan.summary["keep_turns"] == 1
    assert [m.tool_call_id for m in plan.messages if m.role == "tool"] == ["c1"]


def test_over_threshold_without_old_turn_keeps_full() -> None:
    """只有一轮且超阈值时没有可压缩的历史，保持全量并记录原因。"""
    messages = _conversation(1, size=1000)

    plan, outcome = plan_summary_context(
        messages,
        lambda dropped: pytest.fail("不应调用摘要"),
        trigger_chars=10,
    )

    assert outcome is None
    assert plan.messages == messages
    assert plan.summary is not None and plan.summary["reason"] == "no_old_turns"


def test_estimate_context_chars_counts_content_and_calls() -> None:
    messages = [Message(role="system", content="12345"), _call("c1", pattern="abc")]
    assert estimate_context_chars(messages) > len("12345")


class _SummaryClient:
    """只用于摘要客户端测试：记录每次调用，可指定返回文本。"""

    name = "fake"
    model = "fake-summary"

    def __init__(self, text: str | None = "摘要正文") -> None:
        self.text = text
        self.calls: list[dict] = []

    def complete(self, messages, tools=None, max_tokens=None):
        self.calls.append(
            {"messages": list(messages), "tools": tools, "max_tokens": max_tokens}
        )
        return LLMResponse(
            message=Message(role="assistant", content=self.text),
            usage=Usage(input_tokens=10, output_tokens=5, total_tokens=15),
        )


def test_summarizer_uses_fixed_prompt_version_and_cache() -> None:
    """摘要调用必须用固定版本提示与同一客户端，相同内容只计费一次。"""
    client = _SummaryClient()
    summarizer = Summarizer(client, version="v1")
    messages = _conversation(1)[2:]

    first = summarizer.summarize(messages)
    second = summarizer.summarize(messages)

    assert len(client.calls) == 1
    assert first.cache_hit is False and first.response is not None
    assert second.cache_hit is True and second.response is None
    call = client.calls[0]
    assert call["messages"][0].content == SUMMARY_PROMPTS["v1"]
    assert call["tools"] is None
    assert call["max_tokens"] == SUMMARY_MAX_TOKENS
    assert "search_files" in call["messages"][1].content


def test_summarizer_supports_version_rollback() -> None:
    """切到 v0 必须使用 v0 的固定提示，证明可回滚而不是只能前进。"""
    client = _SummaryClient()
    summarizer = Summarizer(client, version="v0")
    summarizer.summarize(_conversation(1)[2:])

    assert SUMMARY_PROMPTS["v0"] != SUMMARY_PROMPTS["v1"]
    assert client.calls[0]["messages"][0].content == SUMMARY_PROMPTS["v0"]


class _LengthThenTextClient:
    """第一次把预算耗尽且无正文，第二次正常返回，模拟推理模型的长输入行为。"""

    name = "fake"
    model = "fake-length"

    def __init__(self) -> None:
        self.max_tokens_seen: list[int] = []

    def complete(self, messages, tools=None, max_tokens=None):
        self.max_tokens_seen.append(max_tokens)
        if len(self.max_tokens_seen) == 1:
            return LLMResponse(
                message=Message(role="assistant", content=None),
                usage=Usage(input_tokens=100, output_tokens=800, total_tokens=900),
                finish_reason="length",
            )
        return LLMResponse(
            message=Message(role="assistant", content="重试后的摘要"),
            usage=Usage(input_tokens=100, output_tokens=200, total_tokens=300),
            finish_reason="stop",
        )


def test_summarizer_retries_empty_length_with_doubled_budget() -> None:
    """空正文且 finish_reason=length 时翻倍预算重试，并按两次调用合计用量。"""
    client = _LengthThenTextClient()
    outcome = Summarizer(client, version="v1", max_tokens=800).summarize(_conversation(1)[2:])

    assert client.max_tokens_seen == [800, 1600]
    assert outcome.text == "重试后的摘要"
    assert outcome.response is not None
    assert outcome.response.attempts == 2
    assert outcome.response.usage.input_tokens == 200
    assert outcome.response.usage.output_tokens == 1000


class _AlwaysLengthClient:
    name = "fake"
    model = "fake-length"

    def __init__(self) -> None:
        self.calls = 0

    def complete(self, messages, tools=None, max_tokens=None):
        self.calls += 1
        return LLMResponse(
            message=Message(role="assistant", content=None),
            usage=Usage(input_tokens=10, output_tokens=max_tokens, total_tokens=10 + max_tokens),
            finish_reason="length",
        )


def test_summarizer_raises_when_both_attempts_are_empty() -> None:
    """重试用尽仍为空时必须报错，让循环降级而不是把空摘要拼进上下文。"""
    client = _AlwaysLengthClient()
    with pytest.raises(SummaryError, match="尝试 2 次") as excinfo:
        Summarizer(client, version="v1", max_tokens=800).summarize(_conversation(1)[2:])
    assert client.calls == 2
    assert excinfo.value.usage.total_tokens == 2420
    assert excinfo.value.attempts == 2
    assert excinfo.value.finish_reason == "length"


def test_summarizer_rejects_unknown_version_and_empty_output() -> None:
    with pytest.raises(ValueError):
        Summarizer(_SummaryClient(), version="v9")
    with pytest.raises(SummaryError):
        Summarizer(_SummaryClient(text=None), version="v1").summarize(_conversation(1)[2:])
    with pytest.raises(SummaryError):
        Summarizer(_SummaryClient(), version="v1").summarize([])


class _SummaryAwareFake(FakeClient):
    """主请求走脚本；摘要请求（tools=None）返回固定文本并单独计数。"""

    def __init__(self, script: list[list[ToolCall]]) -> None:
        super().__init__(script=script)
        self.summary_calls: list[list[Message]] = []
        self.main_seen: list[list[Message]] = []

    def complete(self, messages, tools=None, max_tokens=None):
        if tools is None:
            self.summary_calls.append(list(messages))
            return LLMResponse(
                message=Message(role="assistant", content=f"摘要{len(self.summary_calls)}"),
                usage=Usage(input_tokens=40, output_tokens=10, total_tokens=50),
            )
        self.main_seen.append(list(messages))
        return super().complete(messages, tools, max_tokens)


def _script(steps: int) -> list[list[ToolCall]]:
    calls = [
        [ToolCall(f"c{index}", "search_files", json.dumps({"pattern": f"P{index}"}))]
        for index in range(steps)
    ]
    calls.append([ToolCall("fin", "finish", '{"summary": "完成"}')])
    return calls


def test_run_summary_strategy_counts_usage_and_trace(tmp_path: Path) -> None:
    """摘要调用计入总用量与成本，摘要消息进入主请求，缓存命中不重复调用。"""
    client = _SummaryAwareFake(_script(4))
    tracer = Tracer(path=tmp_path / "traces" / "summary.jsonl")
    result = run(
        "统计",
        client,
        build_registry(tmp_path),
        tracer,
        max_steps=8,
        context_strategy="summary",
        summary_threshold=50,
        summary_version="v1",
    )
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert len(client.summary_calls) == 2, "第 3、5 步应各生成一次摘要，第 4 步命中缓存"
    assert result.usage.total_tokens == 5 * 150 + 2 * 50

    step3 = client.main_seen[2]
    assert step3[1].role == "system" and "摘要1" in step3[1].content
    assert step3[2].role == "user"

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    summary_events = [event for event in events if event["kind"] == "summary"]
    assert len(summary_events) == 2
    assert all(event["version"] == "v1" for event in summary_events)
    assert [event["covered"] for event in summary_events] == [2, 4]

    contexts = [event["context"] for event in events if event["kind"] == "llm"]
    enabled = [context["summary"]["enabled"] for context in contexts]
    assert enabled == [False, False, True, True, True]
    cache_hits = [context["summary"].get("cache_hit") for context in contexts]
    assert cache_hits[2] is False and cache_hits[3] is True

    run_end = next(event for event in reversed(events) if event["kind"] == "run_end")
    assert run_end["usage"]["total"] == 850

    state = json.loads((tmp_path / "traces" / "summary.state.json").read_text(encoding="utf-8"))
    assert state["summary_version"] == "v1"
    assert state["summary_threshold"] == 50


class _FailingSummaryFake(FakeClient):
    """摘要调用必失败，主请求正常，用于验证降级到滑窗。"""

    def __init__(self, script: list[list[ToolCall]]) -> None:
        super().__init__(script=script)
        self.main_seen: list[list[Message]] = []

    def complete(self, messages, tools=None, max_tokens=None):
        if tools is None:
            raise ProviderError("摘要上游故障")
        self.main_seen.append(list(messages))
        return super().complete(messages, tools, max_tokens)


def test_summary_failure_falls_back_to_window(tmp_path: Path) -> None:
    """摘要失败不能终止任务：记录 summary_error，并降级为 window 继续。"""
    client = _FailingSummaryFake(_script(4))
    tracer = Tracer(path=tmp_path / "traces" / "summary-fallback.jsonl")
    result = run(
        "统计",
        client,
        build_registry(tmp_path),
        tracer,
        max_steps=8,
        context_strategy="summary",
        context_window=2,
        summary_threshold=50,
    )
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert [len(messages) for messages in client.main_seen] == [2, 4, 6, 6, 6]
    assert result.context["degraded_to"] == "window"
    assert result.context["summary_error_steps"] == 1

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    summary_errors = [event for event in events if event["kind"] == "summary_error"]
    assert len(summary_errors) == 1, "失败一次后应在本任务内停用摘要"
    assert not any(event["kind"] == "summary" for event in events)


class _EmptySummaryFake(FakeClient):
    """摘要返回空正文并已消耗 token，用于验证失败也如实计费。"""

    def __init__(self, script: list[list[ToolCall]]) -> None:
        super().__init__(script=script)
        self.main_seen: list[list[Message]] = []

    def complete(self, messages, tools=None, max_tokens=None):
        if tools is None:
            raise SummaryError(
                "摘要模型返回空内容",
                usage=Usage(input_tokens=10, output_tokens=800, total_tokens=810),
                attempts=1,
                finish_reason="length",
            )
        self.main_seen.append(list(messages))
        return super().complete(messages, tools, max_tokens)


def test_failed_summary_usage_is_counted(tmp_path: Path) -> None:
    """摘要失败消耗的 token 也必须进入总成本，否则实验数据会低估成本。"""
    client = _EmptySummaryFake(_script(4))
    tracer = Tracer(path=tmp_path / "traces" / "summary-usage.jsonl")
    result = run(
        "统计",
        client,
        build_registry(tmp_path),
        tracer,
        max_steps=8,
        context_strategy="summary",
        context_window=2,
        summary_threshold=50,
    )
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    assert result.stop_reason == "finished"
    assert result.usage.total_tokens == 5 * 150 + 810

    events = [json.loads(line) for line in tracer.path.read_text(encoding="utf-8").splitlines()]
    failure = next(event for event in events if event["kind"] == "summary_error")
    assert failure["usage"]["total"] == 810
    assert failure["cost_yuan"] > 0
    run_end = next(event for event in reversed(events) if event["kind"] == "run_end")
    assert run_end["usage"]["total"] == 5 * 150 + 810


def test_checkpoint_round_trips_summary_settings(tmp_path: Path) -> None:
    """断点必须保存摘要版本与阈值，恢复时才能复现同一策略。"""
    path = tmp_path / "checkpoint.state.json"
    checkpoint = RunCheckpoint(
        run_id="summary",
        trace_path=str(tmp_path / "trace.jsonl"),
        task="任务",
        messages=_conversation(1),
        step=1,
        next_step=1,
        pending_calls=[],
        next_call_index=0,
        usage=Usage(),
        total_cost_yuan=0.0,
        context_strategy="summary",
        summary_version="v0",
        summary_threshold=999,
    )
    checkpoint.save(path)
    loaded = RunCheckpoint.load(path)

    assert loaded.summary_version == "v0"
    assert loaded.summary_threshold == 999

    data = json.loads(path.read_text(encoding="utf-8"))
    data["summary_version"] = "v9"
    path.write_text(json.dumps(data, ensure_ascii=False), encoding="utf-8")
    with pytest.raises(ValueError, match="摘要提示版本"):
        RunCheckpoint.load(path)


def test_cli_passes_summary_settings(monkeypatch, tmp_path: Path, capsys) -> None:
    """CLI 把 --summary-version/--summary-threshold 传进 run，并打印生效值。"""
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
            "summary",
            "--summary-version",
            "v0",
            "--summary-threshold",
            "1234",
        ]
    )
    out = capsys.readouterr().out

    assert code == 0
    assert captured["summary_version"] == "v0"
    assert captured["summary_threshold"] == 1234
    assert "[context] summary（阈值 1234 字符，提示 v0）" in out


def test_cli_rejects_unknown_summary_version() -> None:
    """未知版本在装配前拒绝，避免运行期才报错。"""
    from agent.cli.main import main as cli_main

    with pytest.raises(SystemExit) as excinfo:
        cli_main(["--task", "x", "--provider", "fake", "--summary-version", "v9"])
    assert excinfo.value.code == 2


def test_default_summary_version_is_registered() -> None:
    assert DEFAULT_SUMMARY_VERSION in SUMMARY_PROMPTS
