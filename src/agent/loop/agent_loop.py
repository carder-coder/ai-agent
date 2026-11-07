"""A1 循环：状态机只做四件事——问模型、执行工具、回填结果、判断停。"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from ..llm import LLMClient, Message, ProviderError, Usage
from ..tools import FINISH_TOOL, ToolRegistry
from ..trace import Tracer, cost_yuan

SYSTEM_PROMPT = (
    "你是一个只读的仓库分析智能体。可用工具：read_file、search_files、run_shell、finish。"
    "规则：先看仓库结构再回答；数字必须有工具来源，不许估算；"
    "每步只调一个工具；得到足够信息后必须调用 finish 给出结论，结论里写明数字与依据。"
)

MAX_STEPS_DEFAULT = 12
REPEAT_LIMIT = 3


@dataclass
class RunResult:
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
) -> RunResult:
    messages: list[Message] = [
        Message(role="system", content=SYSTEM_PROMPT),
        Message(role="user", content=task),
    ]
    schemas = registry.schemas()
    last_key: tuple[str, str] | None = None
    repeats = 0
    answer = ""
    stop_reason = "max_steps"

    for step in range(1, max_steps + 1):
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
            answer = resp.message.content or ""
            stop_reason = "no_tool_call"
            break

        for call in resp.message.tool_calls:
            if call.name == FINISH_TOOL:
                try:
                    answer = json.loads(call.arguments).get("summary", "")
                except json.JSONDecodeError:
                    answer = call.arguments
                stop_reason = "finished"
                tracer.event("finish", step=step, answer=answer)
                tracer.record_tool(step, call.name, True, 0, answer)
                break

            key = (call.name, call.arguments)
            repeats = repeats + 1 if key == last_key else 1
            last_key = key
            if repeats >= REPEAT_LIMIT:
                answer = f"连续 {repeats} 次调用同一工具且参数未变，已停止以免空转"
                stop_reason = "no_new_info"
                tracer.event("loop_guard", step=step, tool=call.name, repeats=repeats)
                break

            result = registry.invoke(call.name, call.arguments)
            tracer.record_tool(step, call.name, result.ok, result.elapsed_ms, result.content)
            messages.append(
                Message(
                    role="tool",
                    tool_call_id=call.id,
                    name=call.name,
                    content=("" if result.ok else "ERROR: ") + result.content,
                )
            )

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
