"""A1 循环：状态机只做四件事——问模型、执行工具、回填结果、判断停。"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from ..llm import LLMClient, Message, ProviderError, Usage
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


def run(
    task: str,
    client: LLMClient,
    registry: ToolRegistry,
    tracer: Tracer,
    max_steps: int = MAX_STEPS_DEFAULT,
    system_prompt: str = SYSTEM_PROMPT,
) -> RunResult:
    """运行最多 max_steps 轮的工具循环，并在每次模型/工具交互时写 trace。"""
    messages: list[Message] = [
        Message(role="system", content=system_prompt),
        Message(role="user", content=task),
    ]
    schemas = registry.schemas()
    last_key: tuple[str, str] | None = None
    repeats = 0
    answer = ""
    stop_reason = "max_steps"

    for step in range(1, max_steps + 1):
        # 每轮先问模型；上游错误明确终止本次运行，不伪装成正常答案。
        try:
            resp = client.complete(messages, tools=schemas)
        except ProviderError as exc:
            answer = f"上游不可用：{exc}"
            stop_reason = "provider_error"
            tracer.event("provider_error", step=step, error=str(exc))
            break

        tracer.record_llm(step, getattr(client, "model", "unknown"), resp)
        messages.append(resp.message)

        if not resp.message.tool_calls:
            # 没有工具调用代表模型直接给出文本结论，但不等同于显式 finish。
            answer = resp.message.content or ""
            stop_reason = "no_tool_call"
            break

        for call in resp.message.tool_calls:
            if call.name == FINISH_TOOL:
                # finish 是协议级控制信号，不作为普通工具继续回填给模型。
                try:
                    answer = json.loads(call.arguments).get("summary", "")
                except json.JSONDecodeError:
                    answer = call.arguments
                stop_reason = "finished"
                tracer.event("finish", step=step, answer=answer)
                tracer.record_tool(step, call.name, True, 0, answer, call.arguments)
                break

            key = (call.name, call.arguments)
            # 只比较相邻调用，避免模型短暂换过工具后仍被误判为重复循环。
            repeats = repeats + 1 if key == last_key else 1
            last_key = key
            if repeats >= REPEAT_LIMIT:
                answer = f"连续 {repeats} 次调用同一工具且参数未变，已停止以免空转"
                stop_reason = "no_new_info"
                tracer.event("loop_guard", step=step, tool=call.name, repeats=repeats)
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

        # 达到循环保护条件或收到 finish 后，跳出外层模型调用循环。
        if stop_reason in {"finished", "no_new_info"}:
            break
    else:
        stop_reason = "max_steps"

    result_usage: Usage = tracer.total_usage
    return RunResult(
        answer=answer,
        steps=step,
        stop_reason=stop_reason,
        usage=result_usage,
        cost_yuan=cost_yuan(result_usage),
        trace_path=str(tracer.path),
    )
