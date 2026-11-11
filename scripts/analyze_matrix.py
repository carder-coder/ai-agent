"""汇总 50×4 矩阵：完成率、拒绝正确率、主请求/附加调用 token、成本、步数、单次请求 P95。

成功口径统一来自 metrics.py；表格把主请求输入与摘要等附加调用拆开，
延迟按 trace 里每次模型请求的 latency_ms 统计，而不是整例墙钟。
"""

from __future__ import annotations

import argparse
import glob
import json
import sys
from collections import defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from metrics import (  # noqa: E402 同目录脚本复用口径
    effective_state,
    enrich_record,
    layered_success,
    load_refusal_rules,
    p95,
)
from metrics import (
    is_success as _is_success,
)

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
DEFAULT_INPUTS = (
    "out/matrix_full.jsonl",
    "out/matrix_window.jsonl",
    "out/matrix_summary.jsonl",
    "out/matrix_retrieval.jsonl",
)


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
    """兼容旧调用：完成判定只认 metrics 的口径。"""
    return _is_success(record.get("stop_reason"), record.get("answer"))


def summarize(records: list[dict]) -> list[dict]:
    grouped: dict[str, list[dict]] = defaultdict(list)
    for record in records:
        grouped[record["strategy"]].append(record)
    rows = []
    for strategy in STRATEGY_ORDER:
        group = grouped.get(strategy, [])
        if not group:
            continue
        count = len(group)
        main_inputs = [
            record.get("main_input_tokens") if record.get("main_input_tokens") is not None
            else record["input_tokens"]
            for record in group
        ]
        extras = [record.get("extra_input_tokens") or 0 for record in group]
        latencies = [
            record.get("request_p95_latency_s")
            if record.get("request_p95_latency_s") is not None
            else float(record["elapsed_s"])
            for record in group
        ]
        refusal = [record for record in group if record.get("category") == "refusal"]
        refusal_ok = [record for record in refusal if record.get("refusal_ok")]
        rows.append(
            {
                "strategy": strategy,
                "n": count,
                "success": sum(1 for record in group if is_success(record)),
                "success_rate": sum(1 for record in group if is_success(record)) / count,
                "refusal_n": len(refusal),
                "refusal_ok": len(refusal_ok),
                "avg_main_input": sum(main_inputs) / count,
                "avg_extra_input": sum(extras) / count,
                "avg_input_tokens": sum(record["input_tokens"] for record in group) / count,
                "avg_cost": sum(record["cost_yuan"] for record in group) / count,
                "avg_steps": sum(record["steps"] for record in group) / count,
                "p95_latency": p95([float(value) for value in latencies]),
                "trimmed": sum(1 for record in group if effective_state(record) == "trimmed"),
                "degraded": sum(1 for record in group if effective_state(record) == "degraded"),
            }
        )
    return rows


def markdown_table(rows: list[dict]) -> str:
    lines = [
        "| 策略 | 样本 | 完成率 | 拒绝正确率 | 主请求输入 token | 附加调用输入 token "
        "| 平均成本（元） | 平均步数 | 单次请求 P95（s） |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        refusal = (
            f"{row['refusal_ok']}/{row['refusal_n']}"
            if row["refusal_n"]
            else "n/a"
        )
        lines.append(
            f"| {row['strategy']} | {row['n']} | "
            f"{row['success_rate'] * 100:.1f}% ({row['success']}/{row['n']}) | "
            f"{refusal} | {row['avg_main_input']:.0f} | {row['avg_extra_input']:.0f} | "
            f"{row['avg_cost']:.6f} | {row['avg_steps']:.2f} | {row['p95_latency']:.2f} |"
        )
    return "\n".join(lines)


def layer_table(records: list[dict]) -> str:
    """按 未触发/生效/降级 分层给完成率，避免降级样本混进策略成绩。"""
    layered = layered_success(records)
    lines = [
        "| 策略 | 未触发（完成/样本） | 真正裁剪（完成/样本） | 降级（完成/样本） |",
        "| --- | ---: | ---: | ---: |",
    ]
    for strategy in STRATEGY_ORDER:
        states = layered.get(strategy, {})
        cells = []
        for state in ("untouched", "trimmed", "degraded"):
            bucket = states.get(state)
            cells.append(f"{bucket['success']}/{bucket['n']}" if bucket else "0/0")
        lines.append(f"| {strategy} | " + " | ".join(cells) + " |")
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
    strategies = {record["strategy"] for record in records}
    if strategies != set(STRATEGY_ORDER):
        problems.append(f"策略应为 {list(STRATEGY_ORDER)}，实际 {sorted(strategies)}")
    cases = {record["case_id"] for record in records}
    if expected % len(STRATEGY_ORDER) == 0:
        expected_cases = expected // len(STRATEGY_ORDER)
        if len(cases) != expected_cases:
            problems.append(f"用例数应为 {expected_cases}，实际 {len(cases)}")
        for case_id in cases:
            got = {record["strategy"] for record in records if record["case_id"] == case_id}
            if got != set(STRATEGY_ORDER):
                problems.append(f"用例 {case_id} 缺策略：{sorted(set(STRATEGY_ORDER) - got)}")
                break
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
                        "budget_tokens",
                        "selected",
                        "dropped",
                        "selected_indexes",
                        "query_terms",
                        "over_budget",
                        "truncated_turns",
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
        default=list(DEFAULT_INPUTS),
        help="本次实验的四个结果 JSONL，支持通配符；不要混入历史中间文件或已合并文件",
    )
    parser.add_argument("--merged", default="out/matrix_200.jsonl", help="合并后的输出")
    parser.add_argument("--expect", type=int, default=200, help="期望记录数")
    parser.add_argument("--badcases", type=int, default=0, help="打印前 N 条失败证据")
    parser.add_argument("--out-md", default="", help="把对比表与分层表写入 Markdown 文件")
    args = parser.parse_args()

    paths: list[Path] = []
    for pattern in args.inputs:
        paths.extend(sorted(Path(path) for path in glob.glob(pattern)))
    if not paths:
        parser.error("没有找到矩阵结果文件")
    merged_path = Path(args.merged).resolve()
    if any(path.resolve() == merged_path for path in paths):
        parser.error("--inputs 不能包含合并输出本身，否则会读到上一次合并的旧记录")

    try:
        records = load_records(paths)
    except (OSError, KeyError, ValueError) as exc:
        parser.error(str(exc))

    rules = load_refusal_rules()
    records = [enrich_record(record, rules=rules) for record in records]

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
    layers = layer_table(records)
    print()
    print(layers)
    if args.out_md:
        out_md = Path(args.out_md)
        out_md.parent.mkdir(parents=True, exist_ok=True)
        out_md.write_text(table + "\n\n" + layers + "\n", encoding="utf-8")
        print(f"\n对比表写入 {out_md}")
    if problems:
        print("\n校验问题：")
        for problem in problems:
            print(f"- {problem}")
    print_badcases(records, args.badcases)
    return 1 if problems else 0


if __name__ == "__main__":
    raise SystemExit(main())
