"""命令行入口：一行命令跑完一次任务，trace 落盘并打印成本。"""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path


def load_dotenv(start: Path) -> Path | None:
    """从当前目录向上找 .env，只补缺失的环境变量。"""
    for folder in [start, *start.parents]:
        f = folder / ".env"
        if f.is_file():
            for line in f.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip())
            return f
    return None


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="agent", description="A1 最小闭环命令行")
    parser.add_argument("--task", required=True, help="任务描述")
    parser.add_argument("--max-steps", type=int, default=int(os.environ.get("AGENT_MAX_STEPS", 12)))
    parser.add_argument("--provider", choices=["doubao", "fake"], default="doubao")
    parser.add_argument("--root", default="", help="工具可访问的仓库根目录，默认当前目录")
    parser.add_argument("--quiet", action="store_true", help="只打印结论")
    args = parser.parse_args(argv)

    root = Path(args.root or os.environ.get("AGENT_PROJECT_ROOT") or Path.cwd()).resolve()
    load_dotenv(root)

    from ..llm import DoubaoClient, FakeClient, ProviderError
    from ..loop import run
    from ..tools import build_registry
    from ..trace import Tracer

    if args.provider == "doubao":
        try:
            client = DoubaoClient()
        except ProviderError as exc:
            print(f"无法初始化模型：{exc}")
            return 2
    else:
        client = FakeClient()

    registry = build_registry(root)
    tracer = Tracer()
    result = run(args.task, client, registry, tracer, max_steps=args.max_steps)
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    if not args.quiet:
        print(f"[provider] {client.name}/{client.model}")
        print(f"[steps] {result.steps}（{result.stop_reason}）")
        print(
            f"[tokens] 输入 {result.usage.input_tokens} / 输出 {result.usage.output_tokens}"
            f" / 合计 {result.usage.total_tokens}"
        )
        print(f"[cost] {result.cost_yuan:.6f} 元")
        print(f"[trace] {result.trace_path}")
    print("[answer]")
    print(result.answer or "（模型没有给出结论）")
    return 0 if result.stop_reason == "finished" else 1


if __name__ == "__main__":
    sys.exit(main())
