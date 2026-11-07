"""工具装配：注册四个只读工具，单测与 cli 都从这里取。"""

from __future__ import annotations

from pathlib import Path

from .finish import FinishArgs, finish
from .fs import ReadFileArgs, SearchFilesArgs, make_read_file, make_search_files
from .registry import ToolRegistry, ToolResult
from .shell import RunShellArgs, make_run_shell

FINISH_TOOL = "finish"


def build_registry(root: Path) -> ToolRegistry:
    reg = ToolRegistry(root=root.resolve())

    reg.register(
        "read_file",
        "读取仓库内某个文件的片段，返回带行号的文本，最多 200 行。",
        ReadFileArgs,
    )(make_read_file(reg.root))

    reg.register(
        "search_files",
        "在仓库内按正则搜索文本，返回文件:行号:内容，用于统计与定位。",
        SearchFilesArgs,
    )(make_search_files(reg.root))

    reg.register(
        "run_shell",
        "执行白名单内的只读命令（git status、rg、python 等），超时 10 秒。",
        RunShellArgs,
    )(make_run_shell(reg.root))

    reg.register(
        "finish",
        "任务完成时调用，传入最终结论；每个任务必须且只用一次。",
        FinishArgs,
    )(finish)

    return reg


__all__ = ["FINISH_TOOL", "ToolRegistry", "ToolResult", "build_registry"]
