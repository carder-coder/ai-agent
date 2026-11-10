"""A1 循环：状态机只做四件事——问模型、执行工具、回填结果、判断停。"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from ..context import (
    DEFAULT_CONTEXT_STRATEGY,
    DEFAULT_SUMMARY_VERSION,
    DEFAULT_WINDOW_TURNS,
    RETRIEVAL_BUDGET_CHARS,
    RETRIEVAL_KEEP_RECENT,
    STRATEGIES,
    SUMMARY_KEEP_TURNS,
    SUMMARY_PROMPTS,
    SUMMARY_TRIGGER_CHARS,
    Summarizer,
    SummaryError,
    plan_context,
    plan_retrieval_context,
    plan_summary_context,
)
from ..llm import LLMClient, Message, ProviderError, ToolCall, Usage
from ..tools import FINISH_TOOL, ToolRegistry
from ..trace import Tracer, cost_yuan

SYSTEM_PROMPT = (
    "你是只读仓库分析智能体，只用 read_file、search_files、run_shell、finish。\n"
    "1. 每步一个工具，禁止并发、重复相同参数。\n"
    "2. 统计按匹配行数，遵循用户扫描范围。多个关键词分别用 summary=true 汇总，并集用组合正则；"
    "summary全量总数为准，只在用户要明细时以limit取明细，禁止再用read_file/run_shell复核。\n"
    "3. “已截断”/“只列前 N 条”必须缩小范围重查，不得据此报告总数。\n"
    "4. 优先 search_files，少用 run_shell。\n"
    "5. Windows run_shell仅接受单条命令，不能用管道、重定向、分号、&&、通配符展开；"
    "计数用 python -c。\n"
    "6. 信息足够立即 finish，说明数字、口径和依据；不确定需明说。"
)

CONCISE_SYSTEM_PROMPT = (
    "你是仓库分析助手，每步只调用一个工具。统计按行计数且遵循同一口径；用 search_files "
    "summary=true 查总数，用户要明细才设 limit。汇总已扫全范围，不必重复核对。信息足够立即 "
    "finish，说明数字、口径与依据。"
)

MAX_STEPS_DEFAULT = 12
REPEAT_LIMIT = 3


@dataclass
class RunResult:
    """一次 agent 运行的结论、停止原因和累计成本信息。"""

    answer: str
    steps: int
    stop_reason: str
    usage: Usage = field(default_factory=Usage)
    cost_yuan: float = 0.0
    trace_path: str = ""


@dataclass
class RunCheckpoint:
    """可恢复循环所需的对话、累计计量和未完成工具调用。"""

    run_id: str
    trace_path: str
    task: str
    messages: list[Message]
    step: int
    next_step: int
    pending_calls: list[ToolCall]
    next_call_index: int
    usage: Usage
    total_cost_yuan: float
    last_key: tuple[str, str] | None = None
    repeats: int = 0
    status: str = "in_progress"
    context_strategy: str = DEFAULT_CONTEXT_STRATEGY
    context_window: int = DEFAULT_WINDOW_TURNS
    summary_version: str = DEFAULT_SUMMARY_VERSION
    summary_threshold: int = SUMMARY_TRIGGER_CHARS
    retrieval_budget: int = RETRIEVAL_BUDGET_CHARS

    @classmethod
    def load(cls, path: Path) -> RunCheckpoint:
        """读取并验证状态文件；损坏状态显式报错，不从空对话静默续跑。"""
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
            if data.get("version") != 1:
                raise ValueError(f"不支持的断点版本：{data.get('version')}")
            usage = Usage(**data["usage"])
            messages = [
                Message(
                    role=item["role"],
                    content=item.get("content"),
                    tool_calls=[
                        ToolCall(
                            id=call["id"],
                            name=call["function"]["name"],
                            arguments=call["function"]["arguments"],
                        )
                        for call in item.get("tool_calls", [])
                    ],
                    tool_call_id=item.get("tool_call_id"),
                    name=item.get("name"),
                )
                for item in data["messages"]
            ]
            pending_calls = [
                ToolCall(**call) for call in data.get("pending_calls", [])
            ]
            last_key_data = data.get("last_key")
            last_key = tuple(last_key_data) if last_key_data else None
            context_strategy = data.get("context_strategy", "full")
            if context_strategy not in STRATEGIES:
                raise ValueError(f"不支持的上下文策略：{context_strategy}")
            context_window = int(data.get("context_window", DEFAULT_WINDOW_TURNS))
            if context_window < 1:
                raise ValueError(f"上下文窗口必须大于 0：{context_window}")
            summary_version = data.get("summary_version", DEFAULT_SUMMARY_VERSION)
            if summary_version not in SUMMARY_PROMPTS:
                raise ValueError(f"不支持的摘要提示版本：{summary_version}")
            summary_threshold = int(data.get("summary_threshold", SUMMARY_TRIGGER_CHARS))
            if summary_threshold < 1:
                raise ValueError(f"摘要阈值必须大于 0：{summary_threshold}")
            retrieval_budget = int(data.get("retrieval_budget", RETRIEVAL_BUDGET_CHARS))
            if retrieval_budget < 1:
                raise ValueError(f"检索预算必须大于 0：{retrieval_budget}")
            checkpoint = cls(
                run_id=data["run_id"],
                trace_path=data["trace_path"],
                task=data["task"],
                messages=messages,
                step=int(data["step"]),
                next_step=int(data["next_step"]),
                pending_calls=pending_calls,
                next_call_index=int(data["next_call_index"]),
                usage=usage,
                total_cost_yuan=float(data["total_cost_yuan"]),
                last_key=last_key,
                repeats=int(data.get("repeats", 0)),
                status=data.get("status", "in_progress"),
                context_strategy=context_strategy,
                context_window=context_window,
                summary_version=summary_version,
                summary_threshold=summary_threshold,
                retrieval_budget=retrieval_budget,
            )
        except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
            raise ValueError(f"断点状态无效：{path}: {exc}") from exc
        if checkpoint.next_call_index > len(checkpoint.pending_calls):
            raise ValueError(f"断点状态无效：工具调用位置越界：{path}")
        return checkpoint

    def save(self, path: Path) -> None:
        """原子替换状态文件，避免中断时留下半截 JSON。"""
        data: dict[str, Any] = {
            "version": 1,
            "run_id": self.run_id,
            "trace_path": self.trace_path,
            "task": self.task,
            "messages": [message.to_openai() for message in self.messages],
            "step": self.step,
            "next_step": self.next_step,
            "pending_calls": [
                {"id": call.id, "name": call.name, "arguments": call.arguments}
                for call in self.pending_calls
            ],
            "next_call_index": self.next_call_index,
            "usage": {
                "input_tokens": self.usage.input_tokens,
                "output_tokens": self.usage.output_tokens,
                "total_tokens": self.usage.total_tokens,
                "cached_tokens": self.usage.cached_tokens,
            },
            "total_cost_yuan": self.total_cost_yuan,
            "last_key": list(self.last_key) if self.last_key else None,
            "repeats": self.repeats,
            "status": self.status,
            "context_strategy": self.context_strategy,
            "context_window": self.context_window,
            "summary_version": self.summary_version,
            "summary_threshold": self.summary_threshold,
            "retrieval_budget": self.retrieval_budget,
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_name(path.name + ".tmp")
        temporary.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        # 并发跑批时 Windows 偶尔短暂占用 .tmp，重试几次避免整条用例失败。
        for attempt in range(3):
            try:
                temporary.replace(path)
                return
            except PermissionError:
                if attempt == 2:
                    raise
                time.sleep(0.05 * (attempt + 1))


def run(
    task: str,
    client: LLMClient,
    registry: ToolRegistry,
    tracer: Tracer,
    max_steps: int = MAX_STEPS_DEFAULT,
    system_prompt: str = SYSTEM_PROMPT,
    checkpoint: RunCheckpoint | None = None,
    context_strategy: str | None = None,
    context_window: int | None = None,
    summary_version: str | None = None,
    summary_threshold: int | None = None,
    retrieval_budget: int | None = None,
) -> RunResult:
    """运行最多 max_steps 轮的工具循环，并在每次模型/工具交互时写 trace。

    context_strategy 只影响每次请求携带的消息；messages 始终保存全量对话，
    因此断点恢复、重复检测和工具回填不受策略影响。未显式指定时沿用断点设置。
    """
    if context_strategy is None:
        context_strategy = (
            checkpoint.context_strategy if checkpoint else DEFAULT_CONTEXT_STRATEGY
        )
    if context_window is None:
        context_window = checkpoint.context_window if checkpoint else DEFAULT_WINDOW_TURNS
    if summary_version is None:
        summary_version = checkpoint.summary_version if checkpoint else DEFAULT_SUMMARY_VERSION
    if summary_threshold is None:
        summary_threshold = (
            checkpoint.summary_threshold if checkpoint else SUMMARY_TRIGGER_CHARS
        )
    if summary_version not in SUMMARY_PROMPTS:
        raise ValueError(f"未知摘要提示版本：{summary_version}")
    if summary_threshold < 1:
        raise ValueError(f"摘要阈值必须大于 0，收到 {summary_threshold}")
    if retrieval_budget is None:
        retrieval_budget = (
            checkpoint.retrieval_budget if checkpoint else RETRIEVAL_BUDGET_CHARS
        )
    if retrieval_budget < 1:
        raise ValueError(f"检索预算必须大于 0，收到 {retrieval_budget}")
    summarizer = (
        Summarizer(client, version=summary_version) if context_strategy == "summary" else None
    )
    messages = checkpoint.messages if checkpoint else [
        Message(role="system", content=system_prompt),
        Message(role="user", content=task),
    ]
    schemas = registry.schemas()
    last_key = checkpoint.last_key if checkpoint else None
    repeats = checkpoint.repeats if checkpoint else 0
    pending_calls = checkpoint.pending_calls if checkpoint else []
    next_call_index = checkpoint.next_call_index if checkpoint else 0
    step = checkpoint.step if checkpoint else 0
    next_step = checkpoint.next_step if checkpoint else 1
    answer = ""
    stop_reason = "max_steps"
    checkpoint_path = tracer.path.with_suffix(".state.json")

    def save_checkpoint(status: str = "in_progress") -> None:
        state = RunCheckpoint(
            run_id=tracer.run_id,
            trace_path=str(tracer.path.resolve()),
            task=task,
            messages=messages,
            step=step,
            next_step=next_step,
            pending_calls=pending_calls,
            next_call_index=next_call_index,
            usage=tracer.total_usage,
            total_cost_yuan=tracer.total_cost,
            last_key=last_key,
            repeats=repeats,
            status=status,
            context_strategy=context_strategy,
            context_window=context_window,
            summary_version=summary_version,
            summary_threshold=summary_threshold,
            retrieval_budget=retrieval_budget,
        )
        state.save(checkpoint_path)

    if checkpoint is None:
        save_checkpoint()

    for current_step in range(next_step, max_steps + 1):
        step = current_step
        if not pending_calls:
            # 请求开始前落点；此时中断可安全地重发尚未得到响应的模型请求。
            next_step = step
            save_checkpoint()
            if summarizer is not None and summarizer.disabled_reason is None:
                try:
                    plan, summary_outcome = plan_summary_context(
                        messages,
                        summarizer.summarize,
                        keep_turns=SUMMARY_KEEP_TURNS,
                        trigger_chars=summary_threshold,
                        version=summary_version,
                    )
                except (ProviderError, SummaryError) as exc:
                    # 摘要失败不终止任务：本任务内停用摘要并降级为滑窗。
                    summarizer.disable(str(exc))
                    tracer.record_summary_error(
                        step,
                        getattr(client, "model", "unknown"),
                        getattr(exc, "usage", Usage()),
                        version=summary_version,
                        attempts=getattr(exc, "attempts", 0),
                        finish_reason=getattr(exc, "finish_reason", ""),
                        error=str(exc),
                    )
                    save_checkpoint()
                    plan = plan_context(
                        messages,
                        strategy="window",
                        window_turns=context_window,
                    )
                    summary_outcome = None
                if summary_outcome is not None and summary_outcome.response is not None:
                    tracer.record_summary(
                        step,
                        getattr(client, "model", "unknown"),
                        summary_outcome.response,
                        version=summary_outcome.version,
                        covered=summary_outcome.covered_messages,
                    )
                    # 摘要调用已产生真实费用，先落一次断点再发主请求。
                    save_checkpoint()
            elif summarizer is not None:
                plan = plan_context(
                    messages,
                    strategy="window",
                    window_turns=context_window,
                )
            elif context_strategy == "retrieval":
                plan = plan_retrieval_context(
                    messages,
                    budget_chars=retrieval_budget,
                    keep_recent=RETRIEVAL_KEEP_RECENT,
                )
            else:
                plan = plan_context(
                    messages,
                    strategy=context_strategy,
                    window_turns=context_window,
                )
            try:
                resp = client.complete(plan.messages, tools=schemas)
            except ProviderError as exc:
                answer = f"上游不可用：{exc}"
                stop_reason = "provider_error"
                tracer.event("provider_error", step=step, error=str(exc))
                save_checkpoint(status=stop_reason)
                break

            tracer.record_llm(
                step,
                getattr(client, "model", "unknown"),
                resp,
                context=plan.to_trace(),
            )
            messages.append(resp.message)
            pending_calls = list(resp.message.tool_calls)
            next_call_index = 0
            save_checkpoint()

            if not pending_calls:
                # 没有工具调用代表模型直接给出文本结论，但不等同于显式 finish。
                answer = resp.message.content or ""
                stop_reason = "no_tool_call"
                next_step = step + 1
                save_checkpoint(status=stop_reason)
                break

        for call_index in range(next_call_index, len(pending_calls)):
            call = pending_calls[call_index]
            if call.name == FINISH_TOOL:
                # finish 是协议级控制信号，不作为普通工具继续回填给模型。
                try:
                    answer = json.loads(call.arguments).get("summary", "")
                except json.JSONDecodeError:
                    answer = call.arguments
                stop_reason = "finished"
                tracer.event("finish", step=step, answer=answer)
                tracer.record_tool(step, call.name, True, 0, answer, call.arguments)
                pending_calls = []
                next_call_index = 0
                next_step = step + 1
                save_checkpoint(status=stop_reason)
                break

            key = (call.name, call.arguments)
            # 只比较相邻调用，避免模型短暂换过工具后仍被误判为重复循环。
            repeats = repeats + 1 if key == last_key else 1
            last_key = key
            if repeats >= REPEAT_LIMIT:
                answer = f"连续 {repeats} 次调用同一工具且参数未变，已停止以免空转"
                stop_reason = "no_new_info"
                tracer.event("loop_guard", step=step, tool=call.name, repeats=repeats)
                pending_calls = []
                next_call_index = 0
                next_step = step + 1
                save_checkpoint(status=stop_reason)
                break

            result = registry.invoke(call.name, call.arguments)
            tracer.record_tool(
                step, call.name, result.ok, result.elapsed_ms, result.content, call.arguments
            )
            messages.append(
                Message(
                    role="tool",
                    tool_call_id=call.id,
                    name=call.name,
                    content=("" if result.ok else "ERROR: ") + result.content,
                )
            )
            next_call_index = call_index + 1
            save_checkpoint()

        if stop_reason in {"finished", "no_new_info"}:
            break
        pending_calls = []
        next_call_index = 0
        next_step = step + 1
        save_checkpoint()
    else:
        stop_reason = "max_steps"
        save_checkpoint(status=stop_reason)

    if next_step > max_steps and stop_reason == "max_steps":
        step = max(step, max_steps)

    result_usage: Usage = tracer.total_usage
    return RunResult(
        answer=answer,
        steps=step,
        stop_reason=stop_reason,
        usage=result_usage,
        cost_yuan=cost_yuan(result_usage),
        trace_path=str(tracer.path),
    )
