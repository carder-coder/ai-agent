"""按固定用例集批量运行 agent，一条 --strategy 切换全量拼接与滑窗。

每条结果追加写入 JSONL（`--fresh` 清空重写），包含成功率分析所需的 token、成本、步数与 trace 路径。
默认只跑用例集；先用 --dry-run 核对选择范围，再用 --sample-per-category 做小批冒烟。

用法：
    uv run python scripts/run_cases.py --strategy full --dry-run
    uv run python scripts/run_cases.py --strategy window --sample-per-category 1
    uv run python scripts/run_cases.py --strategy full --provider fake --sample-per-category 1
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import uuid
from collections import Counter
from datetime import datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(ROOT / "scripts"))

from check_cases import CATEGORIES, load_cases  # noqa: E402 同目录脚本复用加载逻辑

from agent.cli.main import load_dotenv  # noqa: E402
from agent.context import (  # noqa: E402
    DEFAULT_SUMMARY_VERSION,
    DEFAULT_WINDOW_TURNS,
    STRATEGIES,
    SUMMARY_TRIGGER_CHARS,
)
from agent.llm import DoubaoClient, FakeClient, ProviderError  # noqa: E402
from agent.loop import run  # noqa: E402
from agent.loop.agent_loop import SYSTEM_PROMPT  # noqa: E402
from agent.tools import build_registry  # noqa: E402
from agent.trace import Tracer  # noqa: E402


def select_cases(
    cases: list[dict],
    category: str | None = None,
    sample_per_category: int = 0,
    limit: int = 0,
) -> list[dict]:
    """按类别过滤、每类抽样，最后截断；保持评测集原有顺序。"""
    selected = [case for case in cases if category is None or case["category"] == category]
    if sample_per_category > 0:
        picked: list[dict] = []
        seen: Counter[str] = Counter()
        for case in selected:
            if seen[case["category"]] < sample_per_category:
                picked.append(case)
                seen[case["category"]] += 1
        selected = picked
    if limit > 0:
        selected = selected[:limit]
    return selected


def _new_client(provider: str, shared_doubao: DoubaoClient | None):
    """fake 每次新建，避免脚本计数器跨用例串台；豆包客户端可复用。"""
    if provider == "fake":
        return FakeClient()
    assert shared_doubao is not None
    return shared_doubao


def main() -> int:
    parser = argparse.ArgumentParser(description="批量运行上下文评测集")
    parser.add_argument(
        "--cases",
        type=Path,
        default=ROOT / "cases" / "context_cases.jsonl",
        help="用例 JSONL 路径",
    )
    parser.add_argument("--strategy", choices=list(STRATEGIES), default="full")
    parser.add_argument(
        "--window",
        type=int,
        default=DEFAULT_WINDOW_TURNS,
        help="window 策略保留的最近轮数",
    )
    parser.add_argument(
        "--summary-version",
        default=DEFAULT_SUMMARY_VERSION,
        help="summary 策略使用的固定提示版本（v1 默认，v0 回滚）",
    )
    parser.add_argument(
        "--summary-threshold",
        type=int,
        default=SUMMARY_TRIGGER_CHARS,
        help="summary 策略触发阈值（上下文字符数）",
    )
    parser.add_argument("--provider", choices=["doubao", "fake"], default="doubao")
    parser.add_argument("--max-steps", type=int, default=12)
    parser.add_argument("--root", default="", help="工具可访问的仓库根目录，默认仓库根")
    parser.add_argument("--category", choices=list(CATEGORIES), help="只跑某一类")
    parser.add_argument("--sample-per-category", type=int, default=0, help="每类取前 N 条")
    parser.add_argument("--limit", type=int, default=0, help="总条数上限，0 表示不限制")
    parser.add_argument("--out", default="out/context_results.jsonl", help="结果 JSONL")
    parser.add_argument("--traces", default="traces", help="trace 输出目录")
    parser.add_argument(
        "--fresh",
        action="store_true",
        help="清空已有结果文件后重写；默认追加，便于多策略写入同一文件",
    )
    parser.add_argument("--dry-run", action="store_true", help="只打印选中的用例，不调用模型")
    args = parser.parse_args()

    if args.window < 1:
        parser.error("--window 必须大于 0")
    if args.summary_threshold < 1:
        parser.error("--summary-threshold 必须大于 0")
    if args.max_steps < 1:
        parser.error("--max-steps 必须大于 0")
    if not args.cases.is_file():
        parser.error(f"用例文件不存在：{args.cases}")

    cases = select_cases(
        load_cases(args.cases),
        category=args.category,
        sample_per_category=args.sample_per_category,
        limit=args.limit,
    )
    if not cases:
        parser.error("没有选中任何用例")

    if args.dry_run:
        for case in cases:
            print(f"{case['category']:10} {case['id']:16} {case['task']}")
        print(f"共 {len(cases)} 条；策略 {args.strategy}，未调用模型")
        return 0

    root = Path(args.root or os.environ.get("AGENT_PROJECT_ROOT") or ROOT).resolve()
    load_dotenv(root)
    out_path = (root / args.out).resolve() if not Path(args.out).is_absolute() else Path(args.out)
    trace_dir = root / args.traces if not Path(args.traces).is_absolute() else Path(args.traces)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    trace_dir.mkdir(parents=True, exist_ok=True)

    shared_doubao: DoubaoClient | None = None
    if args.provider == "doubao":
        try:
            shared_doubao = DoubaoClient()
        except ProviderError as exc:
            print(f"无法初始化模型：{exc}")
            return 2

    registry = build_registry(root)
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    mode = "w" if args.fresh else "a"
    records: list[dict] = []
    errors = 0

    with out_path.open(mode, encoding="utf-8", newline="\n") as sink:
        for index, case in enumerate(cases, 1):
            client = _new_client(args.provider, shared_doubao)
            trace_name = f"{stamp}-{case['id']}-{args.strategy}-{uuid.uuid4().hex[:4]}.jsonl"
            tracer = Tracer(path=trace_dir / trace_name, task=case["task"])
            started = time.perf_counter()
            try:
                result = run(
                    case["task"],
                    client,
                    registry,
                    tracer,
                    max_steps=args.max_steps,
                    system_prompt=SYSTEM_PROMPT,
                    context_strategy=args.strategy,
                    context_window=args.window,
                    summary_version=args.summary_version,
                    summary_threshold=args.summary_threshold,
                )
                elapsed_s = round(time.perf_counter() - started, 3)
                tracer.close(stop_reason=result.stop_reason, steps=result.steps)
                record = {
                    "case_id": case["id"],
                    "category": case["category"],
                    "strategy": args.strategy,
                    "window": args.window if args.strategy == "window" else None,
                    "summary_version": (
                        args.summary_version if args.strategy == "summary" else None
                    ),
                    "summary_threshold": (
                        args.summary_threshold if args.strategy == "summary" else None
                    ),
                    "provider": args.provider,
                    "stop_reason": result.stop_reason,
                    "ok": result.stop_reason == "finished",
                    "steps": result.steps,
                    "input_tokens": result.usage.input_tokens,
                    "output_tokens": result.usage.output_tokens,
                    "cached_tokens": result.usage.cached_tokens,
                    "total_tokens": result.usage.total_tokens,
                    "cost_yuan": round(result.cost_yuan, 6),
                    "elapsed_s": elapsed_s,
                    "trace": str(Path(result.trace_path).resolve()),
                    "answer": result.answer,
                }
            except KeyboardInterrupt:
                tracer.abort(error="KeyboardInterrupt")
                print("已中断；当前结果文件保留已完成记录")
                return 130
            except Exception as exc:  # noqa: BLE001 单条用例失败不应中断整批
                errors += 1
                elapsed_s = round(time.perf_counter() - started, 3)
                tracer.abort(error=f"{type(exc).__name__}: {exc}")
                record = {
                    "case_id": case["id"],
                    "category": case["category"],
                    "strategy": args.strategy,
                    "window": args.window if args.strategy == "window" else None,
                    "summary_version": (
                        args.summary_version if args.strategy == "summary" else None
                    ),
                    "summary_threshold": (
                        args.summary_threshold if args.strategy == "summary" else None
                    ),
                    "provider": args.provider,
                    "stop_reason": "exception",
                    "ok": False,
                    "steps": 0,
                    "input_tokens": 0,
                    "output_tokens": 0,
                    "cached_tokens": 0,
                    "total_tokens": 0,
                    "cost_yuan": 0.0,
                    "elapsed_s": elapsed_s,
                    "trace": str((trace_dir / trace_name).resolve()),
                    "answer": "",
                    "error": f"{type(exc).__name__}: {exc}",
                }

            sink.write(json.dumps(record, ensure_ascii=False) + "\n")
            sink.flush()
            records.append(record)
            status = "ok" if record["ok"] else record["stop_reason"]
            print(
                f"[{index}/{len(cases)}] {case['id']:<16} {status:<12} "
                f"steps={record['steps']:<2} tokens={record['total_tokens']:<6} "
                f"cost={record['cost_yuan']:.6f}元 {record['elapsed_s']:.1f}s"
            )

    finished = sum(record["ok"] for record in records)
    total_cost = sum(record["cost_yuan"] for record in records)
    total_tokens = sum(record["total_tokens"] for record in records)
    print()
    if args.strategy == "window":
        strategy_note = f"（最近 {args.window} 轮）"
    elif args.strategy == "summary":
        strategy_note = f"（阈值 {args.summary_threshold} 字符，提示 {args.summary_version}）"
    else:
        strategy_note = ""
    print(f"策略: {args.strategy}{strategy_note}")
    print(f"完成: {finished}/{len(records)}，异常: {errors}")
    print(f"token 合计: {total_tokens}，成本合计: {total_cost:.6f} 元")
    print(f"结果: {out_path}")
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
