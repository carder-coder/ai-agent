"""循环骨架：用离线替身验证 步数上限、finish 收尾、trace 两类事件。"""

from __future__ import annotations

import json
from pathlib import Path

from agent.llm import FakeClient
from agent.loop import run
from agent.tools import build_registry
from agent.trace import Tracer


def test_loop_finishes(tmp_path: Path) -> None:
    (tmp_path / "a.py").write_text("# TODO one\n", encoding="utf-8")
    tracer = Tracer(path=tmp_path / "traces" / "t.jsonl")
    result = run("统计 TODO", FakeClient(), build_registry(tmp_path), tracer, max_steps=6)
    tracer.close(stop_reason=result.stop_reason)
    assert result.stop_reason == "finished"
    assert "离线冒烟完成" in result.answer
    assert result.steps == 2

    kinds = [
        json.loads(line)["kind"]
        for line in tracer.path.read_text(encoding="utf-8").splitlines()
    ]
    tool_events = [
        json.loads(line)
        for line in tracer.path.read_text(encoding="utf-8").splitlines()
        if json.loads(line)["kind"] == "tool"
    ]
    assert kinds.count("llm") == 2
    assert "tool" in kinds
    assert tool_events[0]["arguments"] == '{"pattern": "TODO|FIXME", "glob": "*.py"}'
    assert kinds[-1] == "run_end"


def test_loop_stops_on_max_steps(tmp_path: Path) -> None:
    from agent.llm import ToolCall

    script = [
        [ToolCall(f"c{i}", "search_files", '{"pattern": "TODO", "glob": "*.py"}')]
        for i in range(5)
    ]
    tracer = Tracer(path=tmp_path / "traces" / "t2.jsonl")
    result = run("死循环", FakeClient(script=script), build_registry(tmp_path), tracer, max_steps=3)
    tracer.close(stop_reason=result.stop_reason)
    assert result.stop_reason == "no_new_info"
    assert result.steps == 3
