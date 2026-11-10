"""校验上下文评测集：条数、必填字段、类别配额与 ID 唯一性。

用法：
    uv run python scripts/check_cases.py
    uv run python scripts/check_cases.py --path cases/context_cases.jsonl --total 50
"""

from __future__ import annotations

import argparse
import json
from collections import Counter
from pathlib import Path

CATEGORIES: tuple[str, ...] = ("single_qa", "multi_tool", "long_read", "refusal")
REQUIRED_FIELDS: tuple[str, ...] = ("id", "category", "task", "expect")
EXPECTED_TOTAL = 50
MAX_IMBALANCE = 1


def load_cases(path: Path) -> list[dict]:
    """逐行解析 JSONL；格式错误立即报出文件名与行号。"""
    cases: list[dict] = []
    with path.open(encoding="utf-8") as fh:
        for line_no, line in enumerate(fh, 1):
            text = line.strip()
            if not text:
                continue
            try:
                item = json.loads(text)
            except json.JSONDecodeError as exc:
                raise ValueError(f"{path}:{line_no}: JSON 无效：{exc}") from exc
            if not isinstance(item, dict):
                raise ValueError(f"{path}:{line_no}: 每行必须是 JSON 对象")
            cases.append(item)
    return cases


def check_cases(cases: list[dict], expected_total: int = EXPECTED_TOTAL) -> list[str]:
    """返回全部错误；空列表代表通过条数、字段、类别与均衡性检查。"""
    errors: list[str] = []
    if len(cases) != expected_total:
        errors.append(f"条数应为 {expected_total}，实际 {len(cases)}")

    ids: Counter[str] = Counter()
    counts: Counter[str] = Counter()
    for index, case in enumerate(cases, 1):
        for field in REQUIRED_FIELDS:
            value = case.get(field)
            if not isinstance(value, str) or not value.strip():
                errors.append(f"第 {index} 条缺少非空字段 {field}")
        category = case.get("category")
        if category not in CATEGORIES:
            errors.append(f"第 {index} 条类别非法：{category!r}（可选：{'、'.join(CATEGORIES)}）")
        elif isinstance(category, str):
            counts[category] += 1
        case_id = case.get("id")
        if isinstance(case_id, str) and case_id:
            ids[case_id] += 1

    duplicates = sorted(case_id for case_id, times in ids.items() if times > 1)
    if duplicates:
        errors.append(f"ID 重复：{'、'.join(duplicates)}")

    missing = [category for category in CATEGORIES if counts[category] == 0]
    if missing:
        errors.append(f"缺少类别：{'、'.join(missing)}")
    elif max(counts.values()) - min(counts.values()) > MAX_IMBALANCE:
        errors.append(
            f"类别不均衡：{dict(counts)}，最大差值应不超过 {MAX_IMBALANCE}"
        )
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description="校验上下文评测集 JSONL")
    parser.add_argument(
        "--path",
        type=Path,
        default=Path(__file__).resolve().parents[1] / "cases" / "context_cases.jsonl",
        help="评测集路径",
    )
    parser.add_argument("--total", type=int, default=EXPECTED_TOTAL, help="期望条数")
    args = parser.parse_args()

    if not args.path.is_file():
        parser.error(f"文件不存在：{args.path}")
    try:
        cases = load_cases(args.path)
    except ValueError as exc:
        parser.exit(1, f"{exc}\n")

    errors = check_cases(cases, expected_total=args.total)
    counts = Counter(case.get("category") for case in cases)
    print(f"文件: {args.path}")
    print(f"总数: {len(cases)}")
    for category in CATEGORIES:
        print(f"  {category}: {counts.get(category, 0)}")
    if errors:
        print("校验失败：")
        for error in errors:
            print(f"  - {error}")
        return 1
    print("校验通过：条数、字段、类别与均衡性均符合约定")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
