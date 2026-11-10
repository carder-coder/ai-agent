"""只读工具的行为边界：能读、能搜、越权被拒、白名单外被拒。"""

from __future__ import annotations

import json
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
    """验证读取文件时输出总行数和带行号的内容。"""
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "src/demo.py", "offset": 0, "max_lines": 2}')
    assert out.ok
    assert "共 3 行" in out.content
    assert "1| # TODO 补测试" in out.content


def test_read_file_rejects_outside(repo: Path) -> None:
    """验证读取仓库根目录之外的路径会被安全策略拒绝。"""
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "../secret.txt"}')
    assert not out.ok
    assert "拒绝执行" in out.content


def test_read_file_rejects_too_many_lines(repo: Path) -> None:
    """验证读取行数超过参数上限时返回参数校验失败。"""
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "src/demo.py", "max_lines": 500}')
    assert not out.ok
    assert "参数校验失败" in out.content


def test_read_file_missing_path_is_reported_as_tool_failure(repo: Path) -> None:
    """验证目标文件不存在时工具结果标记为失败，而非返回成功文本。"""
    reg = build_registry(repo)
    out = reg.invoke("read_file", '{"path": "docs/not-exist.md"}')

    assert not out.ok, "读取不存在的文件必须作为工具失败"
    assert out.content.startswith("执行失败：FileNotFoundError")
    assert "文件不存在：docs/not-exist.md" in out.content


def test_search_files_counts(repo: Path) -> None:
    """验证搜索结果包含匹配行总数和对应文件行号。"""
    reg = build_registry(repo)
    out = reg.invoke("search_files", '{"pattern": "TODO|FIXME", "glob": "*.py", "limit": 10}')
    assert out.ok
    assert "命中 2 行" in out.content
    assert "src/demo.py:1" in out.content


def test_search_files_total_when_limit_hit(repo: Path) -> None:
    """验证达到展示条数上限时仍统计全量命中数并说明截断数量。"""
    (repo / "src" / "many.py").write_text("\n".join(["# TODO"] * 10) + "\n", encoding="utf-8")
    reg = build_registry(repo)
    out = reg.invoke("search_files", '{"pattern": "TODO", "glob": "*.py", "limit": 3}')
    assert out.ok
    assert "命中 11 行" in out.content, "search_files 必须先统计全量命中行数"
    assert "只列前 3 条" in out.content, "超出 limit 时必须明确标注展示条数"


def test_search_files_summary_counts_per_file_without_context(repo: Path) -> None:
    """验证摘要模式按文件统计命中数且不返回匹配上下文。"""
    (repo / "src" / "many.py").write_text("# MATCH\n# MATCH\n", encoding="utf-8")
    reg = build_registry(repo)
    out = reg.invoke(
        "search_files",
        '{"pattern": "MATCH", "glob": "*.py", "summary": true, "limit": 1}',
    )
    assert out.ok
    assert "命中总数 2 行" in out.content
    assert "src/many.py: 2 行" in out.content
    assert ":1:" not in out.content, "summary 不得返回命中上下文"


def test_shell_marks_truncation(repo: Path) -> None:
    """验证 shell 输出超过长度限制时会明确标记内容已截断。"""
    reg = build_registry(repo)
    payload = json.dumps({"command": 'python -c "print(\'x\'*6000)"'})
    out = reg.invoke("run_shell", payload)
    assert out.ok
    assert "已截断" in out.content, "run_shell 超过 4000 字符时必须显式标注截断"


def test_run_shell_blocks_dangerous(repo: Path) -> None:
    """验证危险命令被拒绝，同时允许执行安全的只读状态查询。"""
    reg = build_registry(repo)
    rm = reg.invoke("run_shell", '{"command": "rm -rf src"}')
    push = reg.invoke("run_shell", '{"command": "git push origin main"}')
    assert not rm.ok, "rm 必须被拒绝"
    assert rm.content.startswith("拒绝执行："), "rm 拒绝结果必须使用统一前缀"
    assert not push.ok, "git push 必须被拒绝"
    assert push.content.startswith("拒绝执行："), "git push 拒绝结果必须使用统一前缀"
    out = reg.invoke("run_shell", '{"command": "git status --short"}')
    assert out.ok or "not a git repository" in out.content


def test_finish_marks_done(repo: Path) -> None:
    """验证 finish 工具成功返回带 FINISHED 前缀的完成结果。"""
    reg = build_registry(repo)
    out = reg.invoke("finish", '{"summary": "结论"}')
    assert out.ok
    assert out.content.startswith("FINISHED:")
