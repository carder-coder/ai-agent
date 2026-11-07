"""只读工具的行为边界：能读、能搜、越权被拒、白名单外被拒。"""

from __future__ import annotations

from pathlib import Path

import pytest

from agent.tools import build_registry


@pytest.fixture()
def repo(tmp_path: Path) -> Path:
    (tmp_path / "src").mkdir()
    (tmp_path / "src" / "demo.py").write_text(
        "\n".join(
        [
            "# TODO 补测试",
            "x = 1  # FIXME 改掉",
            "print('hello')",
        ]
        )
        + "\n",
        encoding="utf-8",
    )
    return tmp_path


def test_read_file_has_line_numbers(repo: Path) -> None:
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "src/demo.py", "offset": 0, "max_lines": 2}')
    assert out.ok
    assert "共 3 行" in out.content
    assert "1| # TODO 补测试" in out.content


def test_read_file_rejects_outside(repo: Path) -> None:
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "../secret.txt"}')
    assert not out.ok
    assert "拒绝执行" in out.content


def test_read_file_rejects_too_many_lines(repo: Path) -> None:
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "src/demo.py", "max_lines": 500}')
    assert not out.ok
    assert "参数校验失败" in out.content


def test_search_files_counts(repo: Path) -> None:
    reg = build_registry(repo)
    out = reg.invoke("search_files", '{"pattern": "TODO|FIXME", "glob": "*.py", "limit": 10}')
    assert out.ok
    assert "命中 2 条" in out.content
    assert "src/demo.py:1" in out.content


def test_run_shell_blocks_dangerous(repo: Path) -> None:
    reg = build_registry(repo)
    assert not reg.invoke("run_shell", '{"command": "rm -rf src"}').ok
    assert not reg.invoke("run_shell", '{"command": "git push origin main"}').ok
    out = reg.invoke("run_shell", '{"command": "git status --short"}')
    assert out.ok or "not a git repository" in out.content


def test_finish_marks_done(repo: Path) -> None:
    reg = build_registry(repo)
    out = reg.invoke("finish", '{"summary": "结论"}')
    assert out.ok
    assert out.content.startswith("FINISHED:")
