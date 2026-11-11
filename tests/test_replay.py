"""Trace replay reports step headers before arguments and labels legacy omissions."""

from __future__ import annotations

import json
import runpy
from pathlib import Path

replay = runpy.run_path(str(Path(__file__).parents[1] / "scripts" / "replay.py"))["replay"]


def test_replay_orders_step_header_and_deduplicates_tool_names(
    tmp_path: Path,
    capsys,
) -> None:
    """验证重放先显示步骤再显示参数、去重工具名并标注旧 trace 缺失字段。"""
    trace = tmp_path / "sample.jsonl"
    events = [
        {"kind": "run_start", "run_id": "sample", "ts": "2026-11-09T10:00:00"},
        {
            "kind": "llm",
            "step": 1,
            "usage": {"in": 10, "out": 2, "total": 12},
            "cost_yuan": 0.00001,
            "ts": "2026-11-09T10:00:01",
        },
        {"kind": "tool", "step": 1, "name": "search_files", "arguments": '{"pattern":"A"}'},
        {"kind": "tool", "step": 1, "name": "search_files", "elapsed_ms": 5},
        {
            "kind": "run_end",
            "steps": 1,
            "stop_reason": "finished",
            "usage": {"in": 10, "out": 2, "total": 12},
            "total_cost_yuan": 0.00001,
            "ts": "2026-11-09T10:00:02",
        },
    ]
    trace.write_text(
        "\n".join(json.dumps(event) for event in events) + "\n",
        encoding="utf-8",
    )

    assert replay(trace) == 0
    output = capsys.readouterr().out
    assert output.index("步骤 1:") < output.index("参数:")
    assert "工具=search_files" in output
    assert "工具=search_files, search_files" not in output
    assert "参数缺失（旧 trace 未记录）" in output
    assert "\n\n" not in output


def test_replay_includes_summary_cost_in_total(tmp_path: Path, capsys) -> None:
    """摘要事件也必须参与成本核对，否则重放会误报与 run_end 有差值。"""
    trace = tmp_path / "summary.jsonl"
    events = [
        {"kind": "run_start", "run_id": "summary", "ts": "2026-11-10T10:00:00"},
        {
            "kind": "llm",
            "step": 2,
            "usage": {"in": 100, "out": 10, "total": 110},
            "cost_yuan": 0.001,
            "ts": "2026-11-10T10:00:01",
        },
        {
            "kind": "summary",
            "step": 2,
            "version": "v1",
            "covered": 3,
            "attempts": 1,
            "usage": {"in": 200, "out": 50, "total": 250},
            "cost_yuan": 0.002,
            "ts": "2026-11-10T10:00:02",
        },
        {
            "kind": "run_end",
            "steps": 2,
            "stop_reason": "finished",
            "usage": {"in": 300, "out": 60, "total": 360},
            "total_cost_yuan": 0.003,
            "ts": "2026-11-10T10:00:03",
        },
    ]
    trace.write_text(
        "\n".join(json.dumps(event) for event in events) + "\n",
        encoding="utf-8",
    )

    assert replay(trace) == 0
    output = capsys.readouterr().out
    assert "摘要: v1 覆盖 3 条" in output
    assert "与 run_end 差值: 0.0 元" in output


def test_replay_includes_semantic_cache_cost_in_total(tmp_path: Path, capsys) -> None:
    """语义缓存事件应单独呈现，并参与 run_end 成本复核。"""
    trace = tmp_path / "cache.jsonl"
    events = [
        {"kind": "run_start", "run_id": "cache", "ts": "2026-11-11T10:00:00"},
        {
            "kind": "cache",
            "step": 1,
            "model": "economy",
            "similarity": 1.0,
            "usage": {"in": 1000, "out": 0, "total": 1000, "cached": 1000},
            "cost_yuan": 0.0002,
            "ts": "2026-11-11T10:00:01",
        },
        {
            "kind": "run_end",
            "steps": 1,
            "stop_reason": "cache_hit",
            "usage": {"in": 1000, "out": 0, "total": 1000, "cached": 1000},
            "total_cost_yuan": 0.0002,
            "ts": "2026-11-11T10:00:02",
        },
    ]
    trace.write_text(
        "\n".join(json.dumps(event) for event in events) + "\n",
        encoding="utf-8",
    )

    assert replay(trace) == 0
    output = capsys.readouterr().out
    assert "语义缓存命中: model=economy" in output
    assert "与 run_end 差值: 0.0 元" in output


def test_replay_displays_budget_rejection(tmp_path: Path, capsys) -> None:
    """预算拒绝原因应出现在 trace 重放结果中。"""
    trace = tmp_path / "budget.jsonl"
    events = [
        {"kind": "run_start", "run_id": "budget", "ts": "2026-11-11T10:00:00"},
        {
            "kind": "budget_rejected",
            "estimated_cost_yuan": 0.6,
            "daily_limit_yuan": 100,
            "request_limit_yuan": 0.5,
            "reason": "request_cost_limit_exceeded",
            "ts": "2026-11-11T10:00:01",
        },
        {
            "kind": "run_end",
            "steps": 0,
            "stop_reason": "budget_rejected",
            "usage": {"in": 0, "out": 0, "total": 0},
            "total_cost_yuan": 0,
            "ts": "2026-11-11T10:00:02",
        },
    ]
    trace.write_text(
        "\n".join(json.dumps(event) for event in events) + "\n",
        encoding="utf-8",
    )

    assert replay(trace) == 0
    output = capsys.readouterr().out
    assert "预算拒绝: 估算成本=0.6 元" in output
    assert "request_cost_limit_exceeded" in output
