"""命令行入口：一行命令跑完一次任务，trace 落盘并打印成本。"""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path


def load_dotenv(start: Path) -> Path | None:
    """从 start 向父目录寻找首个 .env；只补缺失变量，不覆盖进程环境。"""
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
    """解析 CLI 参数、装配 provider 和工具，执行一次任务并返回进程退出码。"""
    parser = argparse.ArgumentParser(prog="agent", description="A1 最小闭环命令行")
    parser.add_argument("--task", help="任务描述；断点恢复时可省略")
    parser.add_argument("--resume", help="从 trace JSONL 或对应的 .state.json 断点继续")
    parser.add_argument("--max-steps", type=int, default=int(os.environ.get("AGENT_MAX_STEPS", 12)))
    parser.add_argument("--provider", choices=["doubao", "fake"], default="doubao")
    parser.add_argument(
        "--prompt-variant",
        choices=["detailed", "concise"],
        default="detailed",
        help="系统提示版本，用于同任务对照",
    )
    parser.add_argument(
        "--context-strategy",
        choices=["full", "window", "summary", "retrieval"],
        default=None,
        help="上下文策略：full/window/summary/retrieval。恢复时默认沿用断点",
    )
    parser.add_argument(
        "--context-window",
        type=int,
        default=None,
        help="window 策略保留的最近轮数（一轮=一次响应及其工具结果）",
    )
    parser.add_argument(
        "--summary-version",
        default=None,
        help="摘要提示版本：v1（默认）/v0（回滚用）；恢复时默认沿用断点",
    )
    parser.add_argument(
        "--summary-threshold",
        type=int,
        default=None,
        help="摘要触发阈值（上下文字符数）；恢复时默认沿用断点",
    )
    parser.add_argument(
        "--retrieval-budget",
        type=int,
        default=None,
        help="retrieval 策略的上下文 token 预算；恢复时默认沿用断点",
    )
    parser.add_argument("--root", default="", help="工具可访问的仓库根目录，默认当前目录")
    parser.add_argument("--quiet", action="store_true", help="只打印结论")
    args = parser.parse_args(argv)
    if not args.task and not args.resume:
        parser.error("--task 和 --resume 至少提供一个")

    root = Path(args.root or os.environ.get("AGENT_PROJECT_ROOT") or Path.cwd()).resolve()
    # 先加载仓库配置，再初始化 provider，确保 ARK_* 配置在构造客户端时可见。
    load_dotenv(root)

    from ..context import (
        DEFAULT_CONTEXT_STRATEGY,
        DEFAULT_SUMMARY_VERSION,
        DEFAULT_WINDOW_TURNS,
        RETRIEVAL_BUDGET_TOKENS,
        SUMMARY_PROMPTS,
        SUMMARY_TRIGGER_CHARS,
    )
    from ..llm import DoubaoClient, FakeClient, ProviderError
    from ..loop import RunCheckpoint, run
    from ..loop.agent_loop import CONCISE_SYSTEM_PROMPT, SYSTEM_PROMPT
    from ..tools import build_registry
    from ..trace import Tracer

    checkpoint = None
    resume_source = None
    if args.resume:
        resume_source = Path(args.resume).resolve()
        if resume_source.name.endswith(".state.json"):
            state_path = resume_source
            trace_path = resume_source.with_name(
                resume_source.name.removesuffix(".state.json") + ".jsonl"
            )
        elif resume_source.suffix == ".jsonl":
            state_path = resume_source.with_suffix(".state.json")
            trace_path = resume_source
        else:
            parser.error("--resume 必须指向 .jsonl trace 或 .state.json 文件")
        try:
            checkpoint = RunCheckpoint.load(state_path)
        except ValueError as exc:
            parser.error(str(exc))
        if checkpoint.status in {"finished", "no_tool_call", "no_new_info"}:
            parser.error(f"任务已经结束，不能从此断点恢复：{checkpoint.status}")
        if args.task and args.task != checkpoint.task:
            parser.error("--task 与断点记录的原任务不一致")
        recorded_trace_path = Path(checkpoint.trace_path)
        if recorded_trace_path.resolve() != trace_path:
            parser.error("断点记录的 trace 路径与 --resume 文件不匹配")
        if not trace_path.is_file():
            parser.error(f"原 trace 不存在，无法追加恢复记录：{trace_path}")
        task = checkpoint.task
    else:
        trace_path = None
        task = args.task

    if task is None:
        parser.error("无法从参数或断点获取任务描述")
    # 断点恢复默认沿用原策略，避免一次 --resume 把 window 实验悄悄变回 full。
    if args.context_strategy is None:
        args.context_strategy = (
            checkpoint.context_strategy if checkpoint else DEFAULT_CONTEXT_STRATEGY
        )
    if args.context_window is None:
        args.context_window = (
            checkpoint.context_window if checkpoint else DEFAULT_WINDOW_TURNS
        )
    if args.summary_version is None:
        args.summary_version = (
            checkpoint.summary_version if checkpoint else DEFAULT_SUMMARY_VERSION
        )
    if args.summary_threshold is None:
        args.summary_threshold = (
            checkpoint.summary_threshold
            if checkpoint
            else int(os.environ.get("AGENT_SUMMARY_THRESHOLD", SUMMARY_TRIGGER_CHARS))
        )
    if args.retrieval_budget is None:
        args.retrieval_budget = (
            checkpoint.retrieval_budget
            if checkpoint
            else int(os.environ.get("AGENT_RETRIEVAL_BUDGET", RETRIEVAL_BUDGET_TOKENS))
        )
    if args.max_steps < 1:
        parser.error("--max-steps 必须大于 0")
    if args.context_window < 1:
        parser.error("--context-window 必须大于 0")
    if args.summary_version not in SUMMARY_PROMPTS:
        parser.error(f"--summary-version 必须是：{'、'.join(SUMMARY_PROMPTS)}")
    if args.summary_threshold < 1:
        parser.error("--summary-threshold 必须大于 0")
    if args.retrieval_budget < 1:
        parser.error("--retrieval-budget 必须大于 0")

    if args.provider == "doubao":
        try:
            client = DoubaoClient()
        except ProviderError as exc:
            print(f"无法初始化模型：{exc}")
            return 2
    else:
        client = FakeClient()

    registry = build_registry(root)
    if checkpoint:
        tracer = Tracer(
            path=trace_path,
            run_id=checkpoint.run_id,
            append=True,
            resumed_from=str(resume_source),
            total_usage=checkpoint.usage,
            total_cost=checkpoint.total_cost_yuan,
            task=task,
        )
    else:
        tracer = Tracer(task=task)
    prompt = CONCISE_SYSTEM_PROMPT if args.prompt_variant == "concise" else SYSTEM_PROMPT
    try:
        result = run(
            task,
            client,
            registry,
            tracer,
            max_steps=args.max_steps,
            system_prompt=prompt,
            checkpoint=checkpoint,
            context_strategy=args.context_strategy,
            context_window=args.context_window,
            summary_version=args.summary_version,
            summary_threshold=args.summary_threshold,
            retrieval_budget=args.retrieval_budget,
        )
    except KeyboardInterrupt:
        state_path = tracer.path.with_suffix(".state.json")
        tracer.abort(state_path=str(state_path))
        print(f"任务已中断，可用以下命令恢复：--resume {state_path}")
        return 130
    tracer.close(stop_reason=result.stop_reason, steps=result.steps)

    if not args.quiet:
        print(f"[provider] {client.name}/{client.model}")
        print(f"[steps] {result.steps}（{result.stop_reason}）")
        if args.context_strategy == "summary":
            print(
                f"[context] summary（阈值 {args.summary_threshold} 字符，"
                f"提示 {args.summary_version}）"
            )
        elif args.context_strategy == "retrieval":
            print(f"[context] retrieval（预算 {args.retrieval_budget} token）")
        elif args.context_strategy == "window":
            print(f"[context] window（最近 {args.context_window} 轮）")
        else:
            print("[context] full（全量拼接）")
        print(
            f"[tokens] 输入 {result.usage.input_tokens} / 输出 {result.usage.output_tokens}"
            f" / 合计 {result.usage.total_tokens}（缓存命中 {result.usage.cached_tokens}）"
        )
        print(f"[cost] {result.cost_yuan:.6f} 元")
        print(f"[trace] {result.trace_path}")
    print("[answer]")
    print(result.answer or "（模型没有给出结论）")
    return 0 if result.stop_reason == "finished" else 1


if __name__ == "__main__":
    sys.exit(main())
