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
