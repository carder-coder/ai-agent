"""批量跑批脚本：选择逻辑与 dry-run 不得触发模型调用。"""

from __future__ import annotations

import runpy
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
_runner = runpy.run_path(str(ROOT / "scripts" / "run_cases.py"))
select_cases = _runner["select_cases"]
_checker = runpy.run_path(str(ROOT / "scripts" / "check_cases.py"))
load_cases = _checker["load_cases"]
CASES = load_cases(ROOT / "cases" / "context_cases.jsonl")


def test_sample_per_category_keeps_balance() -> None:
    """每类取一条时正好得到四类各一条，顺序与评测集一致。"""
    selected = select_cases(CASES, sample_per_category=1)

    assert len(selected) == 4
    assert [case["category"] for case in selected] == [
        "single_qa",
        "multi_tool",
        "long_read",
        "refusal",
    ]
    assert [case["id"] for case in selected] == [
        "single-qa-01",
        "multi-tool-01",
        "long-read-01",
        "refusal-01",
    ]


def test_category_filter_and_limit() -> None:
    """类别过滤后可再截断，且不会混入其他类别。"""
    selected = select_cases(CASES, category="refusal", limit=2)

    assert [case["id"] for case in selected] == ["refusal-01", "refusal-02"]
    assert {case["category"] for case in selected} == {"refusal"}


def test_dry_run_does_not_call_model(monkeypatch, capsys) -> None:
    """dry-run 只打印选中的用例，退出码 0。"""
    monkeypatch.setattr(sys, "argv", ["run_cases.py", "--dry-run", "--sample-per-category", "1"])

    assert _runner["main"]() == 0
    out = capsys.readouterr().out
    assert "single-qa-01" in out
    assert "refusal-01" in out
    assert "未调用模型" in out
