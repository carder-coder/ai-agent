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
    p = (root / rel).resolve()
    if root.resolve() not in p.parents and p != root.resolve():
        raise ToolDenied(f"仓库之外的路径：{rel}")
    return p


def make_read_file(root: Path):
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
    def search_files(args: SearchFilesArgs) -> str:
        try:
            rx = re.compile(args.pattern)
        except re.error as exc:
            return f"正则不合法：{exc}"
        hits: list[str] = []
        scanned = 0
        for path in sorted(root.rglob(args.glob)):
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
                    hits.append(f"{path.relative_to(root).as_posix()}:{no}: {line.strip()[:120]}")
                    if len(hits) >= args.limit:
                        break
            if len(hits) >= args.limit:
                break
        if not hits:
            return f"扫描 {scanned} 个文件，没有命中「{args.pattern}」"
        return (
            f"扫描 {scanned} 个文件，命中 {len(hits)} 条（上限 {args.limit}）：\n" + "\n".join(hits)
        )

    return search_files
