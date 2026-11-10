"""汇总 50×4 矩阵：成功率、平均输入 token、平均成本、平均步数、P95 延迟。"""

from __future__ import annotations

import argparse
import glob
import json
import math
from collections import defaultdict
from pathlib import Path

REQUIRED_FIELDS = (
    "case_id",
    "category",
    "strategy",
    "stop_reason",
    "steps",
    "input_tokens",
    "output_tokens",
    "total_tokens",
    "cost_yuan",
    "elapsed_s",
    "trace",
    "answer",
)
STRATEGY_ORDER = ("full", "window", "summary", "retrieval")
SUCCESS_STOP_REASONS = {"finished", "no_tool_call"}


def load_records(paths: list[Path]) -> list[dict]:
    """按 case/strategy 去重；后出现的记录覆盖先出现的，便于补跑失败用例。"""
    by_key: dict[tuple[str, str], dict] = {}
    for path in paths:
        with path.open(encoding="utf-8") as fh:
            for line_no, line in enumerate(fh, 1):
                text = line.strip()
                if not text:
                    continue
                try:
                    record = json.loads(text)
                except json.JSONDecodeError as exc:
                    raise ValueError(f"{path}:{line_no}: JSON 无效：{exc}") from exc
                by_key[(record["case_id"], record["strategy"])] = record
    return list(by_key.values())


def is_success(record: dict) -> bool:
    """程序性成功：给出结论且没有撞上限/上游故障。正确性仍由人工验收。"""
    return (
        record.get("stop_reason") in SUCCESS_STOP_REASONS
        and bool(str(record.get("answer", "")).strip())
    )


def p95(values: list[float]) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = max(0, math.ceil(0.95 * len(ordered)) - 1)
    return ordered[index]


def summarize(records: list[dict]) -> list[dict]:
    grouped: dict[str, list[dict]] = defaultdict(list)
    for record in records:
        grouped[record["strategy"]].append(record)
    rows = []
    for strategy in STRATEGY_ORDER:
        group = grouped.get(strategy, [])
        if not group:
            continue
        successes = [record for record in group if is_success(record)]
        count = len(group)
        rows.append(
            {
                "strategy": strategy,
                "n": count,
                "success": len(successes),
                "success_rate": len(successes) / count if count else 0.0,
                "avg_input_tokens": sum(r["input_tokens"] for r in group) / count,
                "avg_cost": sum(r["cost_yuan"] for r in group) / count,
                "avg_steps": sum(r["steps"] for r in group) / count,
                "p95_latency": p95([float(r["elapsed_s"]) for r in group]),
            }
        )
    return rows


def markdown_table(rows: list[dict]) -> str:
    lines = [
        "| 策略 | 样本 | 成功率 | 平均输入 token | 平均成本（元） | 平均步数 | P95 延迟（s） |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        lines.append(
            f"| {row['strategy']} | {row['n']} | "
            f"{row['success_rate'] * 100:.1f}% ({row['success']}/{row['n']}) | "
            f"{row['avg_input_tokens']:.0f} | {row['avg_cost']:.6f} | "
            f"{row['avg_steps']:.2f} | {row['p95_latency']:.2f} |"
        )
    return "\n".join(lines)


def validate(records: list[dict], expected: int) -> list[str]:
    problems: list[str] = []
    if len(records) != expected:
        problems.append(f"记录数应为 {expected}，实际 {len(records)}")
    missing = sorted(
        {
            f"{record.get('case_id')}/{record.get('strategy')}:{field}"
            for record in records
            for field in REQUIRED_FIELDS
            if field not in record or record[field] is None
        }
    )
    if missing:
        problems.append(f"缺字段 {len(missing)} 处，例如：{missing[:5]}")
    combos = {(record["case_id"], record["strategy"]) for record in records}
    if len(combos) != len(records):
        problems.append("存在重复的 case/strategy 组合")
    return problems


def trace_context_rows(trace_path: Path) -> list[dict]:
    rows: list[dict] = []
    if not trace_path.is_file():
        return rows
    with trace_path.open(encoding="utf-8") as fh:
        for line in fh:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("kind") != "llm":
                continue
            context = event.get("context") or {}
            row = {
                "step": event.get("step"),
                "strategy": context.get("strategy"),
                "used": context.get("used"),
                "total": context.get("total"),
                "dropped": context.get("dropped"),
            }
            if context.get("dropped_detail"):
                row["dropped_detail"] = context["dropped_detail"]
            if context.get("retrieval"):
                row["retrieval"] = {
                    key: context["retrieval"].get(key)
                    for key in (
                        "budget",
                        "selected",
                        "dropped",
                        "selected_indexes",
                        "query_terms",
                        "over_budget",
                    )
                }
            if context.get("summary"):
                row["summary"] = {
                    key: context["summary"].get(key)
                    for key in ("enabled", "covered", "cache_hit", "reason")
                }
            rows.append(row)
    return rows


def summary_event_rows(trace_path: Path) -> list[dict]:
    """摘要成功/失败事件；失败事件可能带已消耗用量。"""
    rows: list[dict] = []
    if not trace_path.is_file():
        return rows
    with trace_path.open(encoding="utf-8") as fh:
        for line in fh:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("kind") not in {"summary", "summary_error"}:
                continue
            rows.append(
                {
                    "kind": event.get("kind"),
                    "step": event.get("step"),
                    "version": event.get("version"),
                    "covered": event.get("covered"),
                    "attempts": event.get("attempts"),
                    "finish_reason": event.get("finish_reason"),
                    "preview": str(event.get("preview") or event.get("error") or "")[:200],
                }
            )
    return rows


def print_badcases(records: list[dict], limit: int) -> None:
    failures = [record for record in records if not is_success(record)]
    print(f"\n失败/非成功记录：{len(failures)} 条")
    for record in failures:
        print(
            f"- {record['case_id']:<16} {record['strategy']:<9} "
            f"{record['stop_reason']:<12} steps={record['steps']} "
            f"trace={record['trace']}"
        )
    if limit <= 0:
        return
    print(f"\n## 前 {limit} 条失败记录的上下文证据")
    for record in failures[:limit]:
        print(f"\n### {record['case_id']} / {record['strategy']}")
        print(f"停止原因: {record['stop_reason']}，步数: {record['steps']}")
        answer = str(record.get("answer", "")).replace("\n", " ")[:200]
        print(f"答案: {answer}")
        for row in trace_context_rows(Path(record["trace"])):
            if not row.get("dropped") and not row.get("summary") and not row.get("retrieval"):
                continue
            print(
                f"步骤 {row['step']}: 保留 {row['used']}/{row['total']}，丢弃 {row['dropped']}"
            )
            if row.get("dropped_detail"):
                print("  被裁: " + json.dumps(row["dropped_detail"], ensure_ascii=False))
            if row.get("retrieval"):
                print("  检索: " + json.dumps(row["retrieval"], ensure_ascii=False))
            if row.get("summary"):
                print("  摘要: " + json.dumps(row["summary"], ensure_ascii=False))
        for event in summary_event_rows(Path(record["trace"])):
            print(
                f"  {event['kind']} 步骤 {event['step']} 版本 {event['version']} "
                f"覆盖 {event['covered']} 尝试 {event['attempts']} "
                f"finish={event['finish_reason']} | {event['preview']}"
            )


def main() -> int:
    parser = argparse.ArgumentParser(description="汇总上下文策略矩阵")
    parser.add_argument(
        "--inputs",
        nargs="+",
        default=["out/matrix_*.jsonl"],
        help="矩阵结果 JSONL，支持通配符",
    )
    parser.add_argument("--merged", default="out/matrix_200.jsonl", help="合并后的输出")
    parser.add_argument("--expect", type=int, default=200, help="期望记录数")
    parser.add_argument("--badcases", type=int, default=0, help="打印前 N 条失败证据")
    parser.add_argument("--out-md", default="", help="把对比表写入 Markdown 文件")
    args = parser.parse_args()

    paths: list[Path] = []
    for pattern in args.inputs:
        matched = sorted(Path(path) for path in glob.glob(pattern))
        paths.extend(matched)
    if not paths:
        parser.error("没有找到矩阵结果文件")

    try:
        records = load_records(paths)
    except (OSError, KeyError, ValueError) as exc:
        parser.error(str(exc))

    problems = validate(records, args.expect)
    if args.merged:
        merged = Path(args.merged)
        merged.parent.mkdir(parents=True, exist_ok=True)
        with merged.open("w", encoding="utf-8", newline="\n") as fh:
            for record in records:
                fh.write(json.dumps(record, ensure_ascii=False) + "\n")
        print(f"合并 {len(records)} 条到 {merged}")

    rows = summarize(records)
    table = markdown_table(rows)
    print()
    print(table)
    if args.out_md:
        out_md = Path(args.out_md)
        out_md.parent.mkdir(parents=True, exist_ok=True)
        out_md.write_text(table + "\n", encoding="utf-8")
        print(f"\n对比表写入 {out_md}")
    if problems:
        print("\n校验问题：")
        for problem in problems:
            print(f"- {problem}")
    print_badcases(records, args.badcases)
    return 1 if problems else 0


if __name__ == "__main__":
    raise SystemExit(main())
