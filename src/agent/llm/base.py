"""Provider 协议：换模型只换实现，循环不感知供应商。"""

from __future__ import annotations

from typing import Protocol

from .types import LLMResponse, Message


class LLMClient(Protocol):
    name: str
    model: str

    def complete(
        self,
        messages: list[Message],
        tools: list[dict] | None = None,
    ) -> LLMResponse: ...
