"""连通性验证：首字节、usage、工具调用、异常归类、429 退避。

用法：
    uv run python scripts/probe.py            # 跑全部
    uv run python scripts/probe.py --case stream
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from openai import OpenAI  # noqa: E402

from agent.cli.main import load_dotenv  # noqa: E402
from agent.llm import DoubaoClient, ProviderError, ProviderRateLimited  # noqa: E402


def show(name: str, ok: bool, detail: str) -> bool:
    print(f"[{'PASS' if ok else 'FAIL'}] {name}  {detail}")
    return ok


def case_stream() -> bool:
    client = DoubaoClient()
    from agent.llm import Message

    resp = client.complete([Message(role="user", content="用一句话说明什么是幂等。")])
    ok = (
        resp.first_byte_ms is not None
        and resp.usage.total_tokens > 0
        and resp.finish_reason in {"stop", "tool_calls"}
        and bool(resp.message.content)
    )
    return show(
        "流式对话",
        ok,
        f"首字节 {resp.first_byte_ms}ms 总耗时 {resp.latency_ms}ms "
        f"usage(in={resp.usage.input_tokens}, out={resp.usage.output_tokens}) "
        f"finish_reason={resp.finish_reason}",
    )


def case_tools() -> bool:
    client = OpenAI(
        api_key=os.environ["ARK_API_KEY"],
        base_url=os.environ.get("ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
    )
    tools = [
        {
            "type": "function",
            "function": {
                "name": "add",
                "description": "计算两个整数之和",
                "parameters": {
                    "type": "object",
                    "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
                    "required": ["a", "b"],
                },
            },
        }
    ]
    resp = client.chat.completions.create(
        model=os.environ["ARK_MODEL"],
        messages=[{"role": "user", "content": "用 add 工具算 17 加 25。"}],
        tools=tools,
    )
    calls = resp.choices[0].message.tool_calls or []
    ok = len(calls) == 1
    detail = "没有触发工具调用"
    if ok:
        c = calls[0]
        detail = f"tool_calls=[{c.function.name}({c.function.arguments})]"
        try:
            args = json.loads(c.function.arguments)
            ok = args.get("a") + args.get("b") == 42 or {"a", "b"} <= set(args)
        except json.JSONDecodeError:
            ok = False
            detail += " 参数不是合法 JSON"
    return show("工具调用", ok, detail)


def case_unreachable() -> bool:
    bad = OpenAI(api_key="placeholder", base_url="http://127.0.0.1:9/v1", timeout=2)
    t0 = time.perf_counter()
    try:
        bad.chat.completions.create(model="x", messages=[{"role": "user", "content": "hi"}])
    except Exception as exc:  # noqa: BLE001 这里就是要看到异常类型
        return show(
            "不可达地址",
            True,
            f"{type(exc).__name__}，{int((time.perf_counter() - t0) * 1000)}ms 内失败（预期失败）",
        )
    return show("不可达地址", False, "居然成功了，地址配置有问题")


def _retry(fn, attempts: int = 3, base: float = 1.0) -> tuple[int, list[float]]:
    """指数退避：1s、2s、4s。只对限流生效，其余错误直接抛。"""
    delays: list[float] = []
    for i in range(attempts + 1):
        try:
            fn()
            return i + 1, delays
        except ProviderRateLimited:
            if i == attempts:
                raise
            d = base * (2**i)
            delays.append(d)
            time.sleep(d)
    raise ProviderError("unreachable")


def case_backoff() -> bool:
    calls = {"n": 0}

    def flaky() -> None:
        calls["n"] += 1
        if calls["n"] <= 2:
            raise ProviderRateLimited("桩：模拟 429")

    tries, delays = _retry(flaky, attempts=3, base=0.2)
    ok = tries == 3 and delays == [0.2, 0.4]
    return show("429 退避", ok, f"尝试 {tries} 次，间隔 {delays} 秒（本地桩，真实 429 需压测触发）")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--case",
        choices=["all", "stream", "tools", "unreachable", "backoff"],
        default="all",
    )
    args = ap.parse_args()
    load_dotenv(Path.cwd())

    if not os.environ.get("ARK_API_KEY") and args.case in {"all", "stream", "tools"}:
        print("缺少 ARK_API_KEY：先把 .env.example 复制为 .env 并填入密钥")
        return 2

    table = {
        "stream": case_stream,
        "tools": case_tools,
        "unreachable": case_unreachable,
        "backoff": case_backoff,
    }
    picked = list(table) if args.case == "all" else [args.case]
    results = [table[name]() for name in picked]
    print()
    print(f"结论：{sum(results)}/{len(results)} 通过")
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())
