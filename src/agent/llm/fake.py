"""离线替身：不发网络请求，用于在没有密钥时验证循环与工具链路。"""

from __future__ import annotations

import json
import time

from .types import LLMResponse, Message, ToolCall, Usage


class FakeClient:
    name = "fake"
    model = "fake-scripted"

    def __init__(self, script: list[list[ToolCall]] | None = None) -> None:
        self.step = 0
        first = json.dumps({"pattern": "TODO|FIXME", "glob": "*.py"}, ensure_ascii=False)
        second = json.dumps({"summary": "离线冒烟完成，工具链路可用"}, ensure_ascii=False)
        self.script = script or [
            [ToolCall("call_1", "search_files", first)],
            [ToolCall("call_2", "finish", second)],
        ]

    def complete(self, messages: list[Message], tools: list[dict] | None = None) -> LLMResponse:
        time.sleep(0.02)
        calls = self.script[self.step] if self.step < len(self.script) else []
        self.step += 1
        return LLMResponse(
            message=Message(role="assistant", content=None, tool_calls=list(calls)),
            usage=Usage(input_tokens=120, output_tokens=30, total_tokens=150),
            finish_reason="tool_calls" if calls else "stop",
            first_byte_ms=20,
            latency_ms=20,
        )
