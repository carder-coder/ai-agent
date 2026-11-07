"""连通性验证：首字节、usage、工具调用、异常归类、429 退避。

用法：
    uv run python scripts/probe.py            # 跑全部
    uv run python scripts/probe.py --case stream
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from openai import OpenAI  # noqa: E402

from agent.cli.main import load_dotenv  # noqa: E402
from agent.llm import DoubaoClient, Message, ProviderRateLimited, ToolCall  # noqa: E402

ADD_TOOL = [
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


def show(name: str, ok: bool, detail: str) -> bool:
    """统一输出单项探测结果，并返回状态供 main 汇总退出码。"""
    print(f"[{'PASS' if ok else 'FAIL'}] {name}  {detail}")
    return ok


def case_stream() -> bool:
    """验证三次真实流式请求的首字节、文本、结束原因和 usage。"""
    client = DoubaoClient()
    ttfts: list[int] = []
    last = None
    for _ in range(3):
        resp = client.complete([Message(role="user", content="用一句话说明什么是幂等。")])
        if resp.first_byte_ms is not None:
            ttfts.append(resp.first_byte_ms)
        last = resp

    assert last is not None
    usage = last.usage
    p50 = int(statistics.median(ttfts)) if ttfts else -1
    ok = (
        bool(ttfts)
        and usage.input_tokens > 0
        and usage.output_tokens > 0
        and usage.total_tokens > 0
        and last.finish_reason in {"stop", "tool_calls"}
        and bool(last.message.content)
    )
    detail = (
        f"首字节 {ttfts} ms，P50 {p50}ms，总耗时 {last.latency_ms}ms，"
        f"usage(in={usage.input_tokens}, out={usage.output_tokens}, total={usage.total_tokens}, "
        f"cached={usage.cached_tokens}) finish_reason={last.finish_reason}"
    )
    print(f"[INFO] 模型 {client.model}")
    if p50 > 1000:
        print(f"[WARN] P50 {p50}ms 超过 1 秒目标：先排除冷启动，换 mini 档再量一次")
    return show("流式对话", ok, detail)


def case_tools() -> bool:
    """验证供应商返回的工具调用经过适配层后仍可正确解析。"""
    client = DoubaoClient()
    resp = client.complete(
        [Message(role="user", content="用 add 工具算 17 加 25。")], tools=ADD_TOOL
    )
    calls: list[ToolCall] = resp.message.tool_calls
    ok = len(calls) == 1 and calls[0].name == "add"
    detail = f"tool_calls={[(c.name, c.arguments) for c in calls]}"
    if ok:
        try:
            args = json.loads(calls[0].arguments)
            ok = args.get("a") == 17 and args.get("b") == 25
        except json.JSONDecodeError:
            ok = False
            detail += " 参数不是合法 JSON（分片拼接有问题）"
        if not ok:
            detail += " 参数不是 17 与 25"
    return show("工具调用（走适配层）", ok, detail)


def case_unreachable() -> bool:
    """确认不可达地址会按预期快速失败，而不是卡住或重试。"""
    bad = OpenAI(api_key="placeholder", base_url="http://127.0.0.1:9/v1", timeout=2, max_retries=0)
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


class _StubHandler(BaseHTTPRequestHandler):
    hits = 0
    fail_times = 2

    def do_POST(self) -> None:  # noqa: N802 标准库要求的名字
        type(self).hits += 1
        if type(self).hits <= type(self).fail_times:
            body = json.dumps(
                {"error": {"message": "stub rate limit", "type": "rate_limit_error", "code": "429"}}
            ).encode()
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        chunks = [
            {"choices": [{"index": 0, "delta": {"content": "ok"}, "finish_reason": None}]},
            {
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
            },
        ]
        payload = "".join(f"data: {json.dumps(c)}\n\n" for c in chunks) + "data: [DONE]\n\n"
        body = payload.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args) -> None:
        return


def case_backoff() -> bool:
    """用本地 HTTP 桩返回两次 429，验证实际客户端退避后能成功恢复。"""
    server = ThreadingHTTPServer(("127.0.0.1", 0), _StubHandler)
    port = server.server_address[1]
    th = threading.Thread(target=server.serve_forever, daemon=True)
    th.start()
    try:
        client = DoubaoClient(
            model="stub", api_key="stub", base_url=f"http://127.0.0.1:{port}/v1", timeout=10
        )
        t0 = time.perf_counter()
        try:
            resp = client.complete([Message(role="user", content="hi")])
        except ProviderRateLimited:
            return show("429 退避", False, f"重试用尽仍被限流，命中 {_StubHandler.hits} 次")
        waited = round(time.perf_counter() - t0, 2)
        ok = (
            _StubHandler.hits == 3
            and client.last_attempts == 3
            and client.last_retry_delays == [1.0, 2.0]
            and resp.usage.total_tokens == 5
        )
        detail = (
            f"命中 {_StubHandler.hits} 次，间隔 {client.last_retry_delays} 秒，"
            f"实际等待 {waited}s，最终 usage total={resp.usage.total_tokens}"
        )
        return show("429 退避（真实客户端 + 本地 429 桩）", ok, detail)
    finally:
        server.shutdown()
        server.server_close()


def main() -> int:
    """选择并执行探测用例；缺少密钥时仅阻止需要真实模型的用例。"""
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

    picked = ["stream", "tools", "unreachable", "backoff"] if args.case == "all" else [args.case]
    table = {
        "stream": case_stream,
        "tools": case_tools,
        "unreachable": case_unreachable,
        "backoff": case_backoff,
    }
    results = [table[name]() for name in picked]
    print()
    print(f"结论：{sum(results)}/{len(results)} 通过")
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())
