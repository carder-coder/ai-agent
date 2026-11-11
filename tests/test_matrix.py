"""矩阵汇总：成功率、均值、P95、去重与字段校验。"""

from __future__ import annotations

import json
import runpy
from pathlib import Path

_matrix = runpy.run_path(
    str(Path(__file__).resolve().parents[1] / "scripts" / "analyze_matrix.py")
)
is_success = _matrix["is_success"]
load_records = _matrix["load_records"]
markdown_table = _matrix["markdown_table"]
p95 = _matrix["p95"]
summarize = _matrix["summarize"]
validate = _matrix["validate"]


def _record(strategy: str, stop: str, answer: str, **overrides) -> dict:
    record = {
        "case_id": overrides.pop("case_id", "case-1"),
        "category": "multi_tool",
        "strategy": strategy,
        "stop_reason": stop,
        "steps": 2,
        "input_tokens": 100,
        "output_tokens": 10,
        "total_tokens": 110,
        "cost_yuan": 0.01,
        "elapsed_s": 1.0,
        "trace": "traces/x.jsonl",
        "answer": answer,
    }
    record.update(overrides)
    return record


def test_success_requires_answer_and_clean_stop() -> None:
    assert is_success(_record("full", "finished", "结论"))
    assert is_success(_record("full", "no_tool_call", "直接回答"))
    assert not is_success(_record("full", "max_steps", "半截"))
    assert not is_success(_record("full", "provider_error", "上游不可用"))
    assert not is_success(_record("full", "finished", "   "))


def test_p95_uses_nearest_rank() -> None:
    assert p95([]) == 0.0
    assert p95([4, 1, 3, 2]) == 4.0
    assert p95([float(value) for value in range(1, 101)]) == 95.0


def test_summarize_computes_five_metrics() -> None:
    records = [
        _record("full", "finished", "a", case_id="c1", input_tokens=100, cost_yuan=0.01,
                steps=2, elapsed_s=1),
        _record("full", "no_tool_call", "b", case_id="c2", input_tokens=200, cost_yuan=0.02,
                steps=4, elapsed_s=2),
        _record("full", "max_steps", "", case_id="c3", input_tokens=300, cost_yuan=0.03,
                steps=6, elapsed_s=3),
        _record("full", "provider_error", "上游不可用", case_id="c4", input_tokens=400,
                cost_yuan=0.04, steps=1, elapsed_s=4),
        _record("window", "finished", "c", case_id="c1", input_tokens=50, cost_yuan=0.005,
                steps=1, elapsed_s=10),
    ]

    rows = {row["strategy"]: row for row in summarize(records)}
    full = rows["full"]

    assert full["n"] == 4
    assert full["success"] == 2
    assert full["success_rate"] == 0.5
    assert full["avg_input_tokens"] == 250
    assert abs(full["avg_cost"] - 0.025) < 1e-9
    assert full["avg_steps"] == 3.25
    assert full["p95_latency"] == 4.0


def test_markdown_table_has_all_columns() -> None:
    table = markdown_table(summarize([_record("full", "finished", "a")]))

    assert "| 策略 | 样本 | 完成率 | 拒绝正确率 | 主请求输入 token |" in table
    assert "| full | 1 | 100.0% (1/1) | n/a | 100 | 0 |" in table
    assert "单次请求 P95（s）" in table


def test_load_records_deduplicates_same_combo(tmp_path: Path) -> None:
    record = _record("full", "finished", "a")
    first = tmp_path / "a.jsonl"
    second = tmp_path / "b.jsonl"
    first.write_text(json.dumps(record, ensure_ascii=False) + "\n", encoding="utf-8")
    second.write_text(json.dumps(record, ensure_ascii=False) + "\n", encoding="utf-8")

    assert len(load_records([first, second])) == 1


def test_validate_reports_missing_field_and_count() -> None:
    records = [_record("full", "finished", "a")]
    problems = validate(records, expected=4)

    assert any("记录数应为 4" in problem for problem in problems)
    assert any("策略应为" in problem for problem in problems)

    broken = _record("full", "finished", "")
    broken.pop("cost_yuan")
    problems = validate([broken], expected=1)
    assert any("cost_yuan" in problem for problem in problems)


def test_validate_requires_every_case_to_have_all_strategies() -> None:
    records = [
        _record("full", "finished", "a", case_id="c1"),
        _record("window", "finished", "a", case_id="c1"),
        _record("summary", "finished", "a", case_id="c1"),
        _record("retrieval", "finished", "a", case_id="c1"),
        _record("full", "finished", "a", case_id="c2"),
    ]
    problems = validate(records, expected=8)

    assert any("缺策略" in problem for problem in problems)
