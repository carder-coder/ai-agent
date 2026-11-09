"""模型适配层：流式分片必须按 index 拼接，名字和参数都不能被后一片覆盖。"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace

from agent.llm import DoubaoClient, Message


def _delta(content=None, tool_calls=None, finish=None):
    return SimpleNamespace(
        choices=[SimpleNamespace(delta=SimpleNamespace(content=content, tool_calls=tool_calls),
                                 finish_reason=finish)]
    )


def _frag(index, name=None, arguments=None, call_id=None):
    return SimpleNamespace(
        index=index,
        id=call_id,
        function=SimpleNamespace(name=name, arguments=arguments),
    )


def test_tool_call_fragments_are_assembled(monkeypatch) -> None:
    client = DoubaoClient(model="stub", api_key="stub", base_url="http://127.0.0.1:9/v1")
    chunks = [
        _delta(tool_calls=[_frag(0, name="ad", arguments='{"a": 1', call_id="call_1")]),
        _delta(tool_calls=[_frag(0, name="d", arguments='7, "b": 25}')]),
        _delta(finish="tool_calls"),
    ]
    monkeypatch.setattr(
        client._client.chat.completions, "create", lambda **kwargs: iter(chunks)
    )
    resp = client.complete([Message(role="user", content="加一下")])

    assert len(resp.message.tool_calls) == 1
    call = resp.message.tool_calls[0]
    assert call.name == "add"
    assert json.loads(call.arguments) == {"a": 17, "b": 25}
    assert resp.attempts == 1


def test_rate_limit_retries_against_local_http_server(monkeypatch) -> None:
    attempts: list[int] = []
    delays: list[float] = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            attempts.append(1)
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            if len(attempts) <= 2:
                body = b'{"error":{"message":"rate limited","type":"rate_limit_error"}}'
                self.send_response(429)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return

            chunks = [
                {
                    "id": "chatcmpl-test",
                    "object": "chat.completion.chunk",
                    "created": 1,
                    "model": "stub",
                    "choices": [{"index": 0, "delta": {"content": "ok"}, "finish_reason": None}],
                },
                {
                    "id": "chatcmpl-test",
                    "object": "chat.completion.chunk",
                    "created": 1,
                    "model": "stub",
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                },
                {
                    "id": "chatcmpl-test",
                    "object": "chat.completion.chunk",
                    "created": 1,
                    "model": "stub",
                    "choices": [],
                    "usage": {
                        "prompt_tokens": 2,
                        "completion_tokens": 1,
                        "total_tokens": 3,
                    },
                },
            ]
            body = "".join(
                f"data: {json.dumps(chunk)}\n\n" for chunk in chunks
            ) + "data: [DONE]\n\n"
            encoded = body.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, format: str, *args) -> None:
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    monkeypatch.setattr("agent.llm.doubao.time.sleep", delays.append)
    client = DoubaoClient(
        model="stub",
        api_key="stub",
        base_url=f"http://127.0.0.1:{server.server_port}/v1",
        max_retries=3,
    )
    try:
        response = client.complete([Message(role="user", content="ping")])
    finally:
        client._client.close()
        server.shutdown()
        server.server_close()
        thread.join()

    assert len(attempts) == 3, "本地桩应收到两次 429 和一次成功 SSE 请求"
    assert response.attempts == 3, "响应 attempts 必须包含所有尝试"
    assert delays == [1.0, 2.0], "429 退避间隔必须依次为 1 秒和 2 秒"
