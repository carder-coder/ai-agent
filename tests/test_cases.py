"""评测集契约：50 条、四类均衡、字段齐全、ID 唯一，且校验脚本能发现问题。"""

from __future__ import annotations

import json
import runpy
from collections import Counter
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
_checker = runpy.run_path(str(ROOT / "scripts" / "check_cases.py"))
CATEGORIES = _checker["CATEGORIES"]
check_cases = _checker["check_cases"]
load_cases = _checker["load_cases"]
CASES_PATH = ROOT / "cases" / "context_cases.jsonl"


def test_frozen_case_set_is_complete_and_balanced() -> None:
    """冻结的 50 条用例必须通过全部契约检查，且四类各占约四分之一。"""
    cases = load_cases(CASES_PATH)
    counts = Counter(case["category"] for case in cases)

    assert len(cases) == 50
    assert set(counts) == set(CATEGORIES)
    assert max(counts.values()) - min(counts.values()) <= 1
    assert counts == Counter({"single_qa": 13, "multi_tool": 13, "long_read": 12, "refusal": 12})
    assert len({case["id"] for case in cases}) == 50
    assert check_cases(cases) == []


def test_every_case_has_usable_task_and_expectation() -> None:
    """任务与验收说明必须可直接用于跑批，不接受占位符。"""
    for case in load_cases(CASES_PATH):
        assert len(case["task"].strip()) >= 10
        assert len(case["expect"].strip()) >= 8
        assert not case["task"].startswith("TODO")


def test_checker_reports_bad_records(tmp_path: Path) -> None:
    """重复 ID、缺字段、非法类别都要被逐条报出，而不是只报总数。"""
    path = tmp_path / "bad.jsonl"
    records = [
        {"id": "dup", "category": "single_qa", "task": "读取文件", "expect": "给出内容"},
        {"id": "dup", "category": "single_qa", "task": "再读一次", "expect": "给出内容"},
        {"id": "no-expect", "category": "bogus", "task": "缺验收"},
    ]
    path.write_text(
        "\n".join(json.dumps(record, ensure_ascii=False) for record in records) + "\n",
        encoding="utf-8",
    )

    errors = check_cases(load_cases(path), expected_total=3)
    text = "\n".join(errors)

    assert "ID 重复：dup" in text
    assert "缺少非空字段 expect" in text
    assert "类别非法" in text
    assert "缺少类别" in text
