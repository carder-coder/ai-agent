"""Replay a JSONL trace as a compact, field-checked report."""

from __future__ import annotations

import argparse
import json
from datetime import datetime
from pathlib import Path
from typing import Any


def _required(record: dict[str, Any], key: str, missing: set[str]) -> Any:
    if key not in record or record[key] is None:
        missing.add(key)
        return None
    return record[key]


def _load_trace(path: Path) -> list[dict[str, Any]]:
    events = []
    with path.open(encoding="utf-8") as trace:
        for line_no, line in enumerate(trace, 1):
            try:
                event = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"{path}:{line_no}: JSON 无效：{exc}") from exc
            if not isinstance(event, dict):
                raise ValueError(f"{path}:{line_no}: trace 事件必须是 JSON 对象")
            events.append(event)
    return events


def replay(path: Path) -> int:
    events = _load_trace(path)
    missing: set[str] = set()
    start = next((event for event in events if event.get("kind") == "run_start"), {})
    end = next((event for event in reversed(events) if event.get("kind") == "run_end"), {})
    run_id = _required(start, "run_id", missing)
    stop_reason = _required(end, "stop_reason", missing)
    llm_events = [event for event in events if event.get("kind") == "llm"]
    tool_events: dict[int, list[dict[str, Any]]] = {}
    for event in events:
        if event.get("kind") == "tool":
            step = _required(event, "step", missing)
            if step is not None:
                tool_events.setdefault(step, []).append(event)

    timestamps = [
        datetime.fromisoformat(event["ts"])
        for event in events
        if isinstance(event.get("ts"), str)
    ]
    elapsed_s = (timestamps[-1] - timestamps[0]).total_seconds() if len(timestamps) >= 2 else None
    if elapsed_s is None:
        missing.add("ts")
    print(f"run_id: {run_id if run_id is not None else '字段缺失：run_id'}")
    print(f"步数: {end.get('steps', len(llm_events))}")
    print(f"stop_reason: {stop_reason if stop_reason is not None else '字段缺失：stop_reason'}")
    print(f"总耗时: {elapsed_s:.3f}s" if elapsed_s is not None else "总耗时: 字段缺失：ts")

    for llm in llm_events:
        step = _required(llm, "step", missing)
        if step is None:
            print("步骤 字段缺失：step")
            continue
        associated_tools = tool_events.get(step, [])
        tool_names: list[str] = []
        for tool in associated_tools:
            name = _required(tool, "name", missing)
            name_text = str(name) if name is not None else "字段缺失：name"
            if name_text not in tool_names:
                tool_names.append(name_text)
        tool_name = ", ".join(tool_names) if tool_names else "无工具事件"
        usage = _required(llm, "usage", missing)
        if not isinstance(usage, dict):
            usage = {}
            missing.add("usage")
        input_tokens = _required(usage, "in", missing)
        output_tokens = _required(usage, "out", missing)
        cost = _required(llm, "cost_yuan", missing)
        print(
            f"步骤 {step}: 工具={tool_name} | "
            f"token={input_tokens if input_tokens is not None else '字段缺失：in'}"
            f"/{output_tokens if output_tokens is not None else '字段缺失：out'} | "
            f"成本={cost if cost is not None else '字段缺失：cost_yuan'} 元"
        )
        context = llm.get("context")
        if isinstance(context, dict):
            print(
                f"  上下文: {context.get('strategy')} "
                f"保留 {context.get('used')}/{context.get('total')} 条，"
                f"丢弃 {context.get('dropped')}"
            )
        for tool in associated_tools:
            args = _required(tool, "arguments", missing)
            if args is None:
                args_text = "参数缺失（旧 trace 未记录）"
            else:
                args_text = str(args).replace("\n", " ")
                if len(args_text) > 80:
                    args_text = args_text[:77] + "..."
            print(
                f"  参数: {args_text} | 工具耗时: "
                f"{tool.get('elapsed_ms', '字段缺失：elapsed_ms')}ms"
            )

    usage_total = _required(end, "usage", missing)
    if not isinstance(usage_total, dict):
        usage_total = {}
        missing.add("usage")
    total_tokens = _required(usage_total, "total", missing)
    total_cost = _required(end, "total_cost_yuan", missing)
    llm_costs = [event.get("cost_yuan") for event in llm_events]
    if any(value is None for value in llm_costs):
        computed_cost = None
        missing.add("cost_yuan")
    else:
        computed_cost = round(sum(llm_costs), 6)
    if total_cost is None or computed_cost is None:
        difference: float | str = "字段缺失：total_cost_yuan/cost_yuan"
    else:
        raw_difference = total_cost - computed_cost
        # Per-call values are rounded before writing; allow their cumulative quantization error.
        precision_tolerance = (len(llm_costs) + 1) * 0.0000005
        difference = 0.0 if abs(raw_difference) <= precision_tolerance else round(
            raw_difference, 6
        )
    print(
        f"总 token: {total_tokens if total_tokens is not None else '字段缺失：total'} | "
        f"总成本: {total_cost if total_cost is not None else '字段缺失：total_cost_yuan'} 元 | "
        f"与 run_end 差值: {difference} 元"
    )
    for field in sorted(missing):
        print(f"字段缺失：{field}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="重放并核对 agent JSONL trace")
    parser.add_argument("--trace", required=True, type=Path, help="trace JSONL 文件路径")
    args = parser.parse_args()
    if not args.trace.is_file():
        parser.error(f"trace 文件不存在：{args.trace}")
    try:
        return replay(args.trace)
    except (OSError, ValueError) as exc:
        parser.exit(1, f"读取 trace 失败：{exc}\n")


if __name__ == "__main__":
    raise SystemExit(main())
