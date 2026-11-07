"""只读文件工具：read_file 与 search_files，全部限制在仓库根目录内。"""

from __future__ import annotations

import re
from pathlib import Path

from pydantic import BaseModel, Field

from .registry import ToolDenied

SKIP_DIRS = {
    ".git",
    ".venv",
    "__pycache__",
    ".pytest_cache",
    ".ruff_cache",
    "node_modules",
    "traces",
    "out",
}


class ReadFileArgs(BaseModel):
    path: str = Field(description="相对仓库根目录的文件路径，例如 src/agent/cli/main.py")
    offset: int = Field(default=0, ge=0, description="从第几行开始读，0 表示文件开头")
    max_lines: int = Field(default=200, ge=1, le=200, description="最多读取行数，上限 200")


class SearchFilesArgs(BaseModel):
    pattern: str = Field(description="正则表达式，例如 TODO|FIXME")
    glob: str = Field(default="*", description="文件匹配模式，例如 *.py 或 **/*.md")
    limit: int = Field(default=20, ge=1, le=60, description="最多返回多少条命中")


def _resolve(root: Path, rel: str) -> Path:
    """解析相对路径并拒绝逃出仓库根目录的路径（含 .. 与符号链接）。"""
    p = (root / rel).resolve()
    if root.resolve() not in p.parents and p != root.resolve():
        raise ToolDenied(f"仓库之外的路径：{rel}")
    return p


def make_read_file(root: Path):
    """构造绑定仓库根目录的读取工具，避免每次调用重复传 root。"""
    def read_file(args: ReadFileArgs) -> str:
        f = _resolve(root, args.path)
        if not f.is_file():
            return f"文件不存在：{args.path}"
        lines = f.read_text(encoding="utf-8", errors="replace").splitlines()
        chunk = lines[args.offset : args.offset + args.max_lines]
        body = "\n".join(f"{i + args.offset + 1:>4}| {ln}" for i, ln in enumerate(chunk))
        head = (
            f"[{args.path}] 共 {len(lines)} 行，"
            f"本次返回第 {args.offset + 1} 至 {args.offset + len(chunk)} 行"
        )
        return f"{head}\n{body}" if body else f"{head}\n（该区间没有内容）"

    return read_file


def make_search_files(root: Path):
    """构造仓库搜索工具；返回列表受 limit 限制，但命中总数始终扫完整个范围。"""
    def search_files(args: SearchFilesArgs) -> str:
        try:
            rx = re.compile(args.pattern)
        except re.error as exc:
            return f"正则不合法：{exc}"
        hits: list[str] = []
        total = 0
        scanned = 0
        for path in sorted(root.rglob(args.glob)):
            # 跳过运行产物和二进制，防止污染统计或把不可读内容当文本搜索。
            if any(part in SKIP_DIRS for part in path.parts) or not path.is_file():
                continue
            if path.suffix.lower() in {".png", ".jpg", ".jpeg", ".gif", ".zip", ".pdf", ".docx"}:
                continue
            try:
                text = path.read_text(encoding="utf-8", errors="ignore")
            except OSError:
                continue
            scanned += 1
            for no, line in enumerate(text.splitlines(), 1):
                if rx.search(line):
                    # total 统计全量命中，hits 只负责保存展示上限内的明细。
                    total += 1
                    if len(hits) < args.limit:
                        rel = path.relative_to(root).as_posix()
                        hits.append(f"{rel}:{no}: {line.strip()[:120]}")
        if not total:
            return f"扫描 {scanned} 个文件，没有命中「{args.pattern}」"
        head = f"扫描 {scanned} 个文件，命中 {total} 行"
        if total > len(hits):
            head += f"，下面只列前 {len(hits)} 条（命中总数以上面的数字为准）"
        return head + "：\n" + "\n".join(hits)

    return search_files
