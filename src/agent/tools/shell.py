"""run_shell：命令白名单 + 超时 + shell=False，危险子命令直接拒绝。"""

from __future__ import annotations

import shlex
import subprocess
from pathlib import Path

from pydantic import BaseModel, Field

from .registry import ToolDenied

ALLOWED_BINS = {"git", "rg", "python", "pytest", "uv", "ruff"}
DENIED_GIT = {"push", "reset", "clean", "filter-branch", "rebase"}
DENIED_BINS = {"rm", "del", "format", "curl", "wget", "shutdown", "reg", "netsh", "ssh"}
TIMEOUT_S = 10
MAX_OUTPUT = 4000


class RunShellArgs(BaseModel):
    command: str = Field(description="要执行的命令，例如 git status --short 或 rg -n TODO src")
    timeout_s: int = Field(default=TIMEOUT_S, ge=1, le=TIMEOUT_S, description="超时秒数，上限 10")


def make_run_shell(root: Path):
    def run_shell(args: RunShellArgs) -> str:
        try:
            argv = shlex.split(args.command, posix=True)
        except ValueError as exc:
            return f"命令无法解析：{exc}"
        if not argv:
            return "命令为空"
        bin_name = Path(argv[0]).name.lower().removesuffix(".exe")
        if bin_name in DENIED_BINS:
            raise ToolDenied(f"{bin_name} 属于危险命令，永久禁止")
        if bin_name not in ALLOWED_BINS:
            raise ToolDenied(f"{bin_name} 不在白名单（{'、'.join(sorted(ALLOWED_BINS))}）")
        if bin_name == "git" and len(argv) > 1 and argv[1] in DENIED_GIT:
            raise ToolDenied(f"git {argv[1]} 会改动远端或历史，需要人工确认")
        try:
            proc = subprocess.run(
                argv,
                cwd=str(root),
                capture_output=True,
                text=True,
                encoding="utf-8",
                errors="replace",
                timeout=args.timeout_s,
                shell=False,
            )
        except subprocess.TimeoutExpired:
            return f"超时：命令在 {args.timeout_s} 秒内没有结束，已终止"
        except FileNotFoundError:
            return f"命令不存在：{argv[0]}"
        out = (proc.stdout or "").strip()
        err = (proc.stderr or "").strip()
        text = f"退出码 {proc.returncode}"
        if out:
            text += f"\n[stdout]\n{out[:MAX_OUTPUT]}"
        if err:
            text += f"\n[stderr]\n{err[:MAX_OUTPUT]}"
        return text

    return run_shell
