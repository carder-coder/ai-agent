"""断点写入：Windows 并发占用 .tmp 时应重试，而不是让整条用例失败。"""

from __future__ import annotations

from pathlib import Path

import pytest

from agent.llm import Message, Usage
from agent.loop import RunCheckpoint


def _checkpoint() -> RunCheckpoint:
    return RunCheckpoint(
        run_id="retry",
        trace_path="traces/retry.jsonl",
        task="任务",
        messages=[
            Message(role="system", content="系统提示"),
            Message(role="user", content="任务"),
        ],
        step=1,
        next_step=1,
        pending_calls=[],
        next_call_index=0,
        usage=Usage(),
        total_cost_yuan=0.0,
    )


def test_save_retries_transient_permission_error(tmp_path: Path, monkeypatch) -> None:
    calls = {"count": 0}
    original = Path.replace

    def flaky_replace(self: Path, target: Path) -> Path:
        calls["count"] += 1
        if calls["count"] == 1:
            raise PermissionError("temporary file is locked")
        return original(self, target)

    monkeypatch.setattr(Path, "replace", flaky_replace)
    path = tmp_path / "state.json"

    _checkpoint().save(path)

    assert path.is_file()
    assert calls["count"] == 2


def test_save_raises_after_three_failures(tmp_path: Path, monkeypatch) -> None:
    calls = {"count": 0}

    def always_denied(self: Path, target: Path) -> Path:
        calls["count"] += 1
        raise PermissionError("still locked")

    monkeypatch.setattr(Path, "replace", always_denied)

    with pytest.raises(PermissionError):
        _checkpoint().save(tmp_path / "state.json")
    assert calls["count"] == 3
