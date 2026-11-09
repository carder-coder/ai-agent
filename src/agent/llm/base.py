"""Provider 协议：换模型只换实现，循环不感知供应商。"""

from __future__ import annotations

from typing import Protocol

from .types import LLMResponse, Message


class LLMClient(Protocol):
    """循环依赖的最小模型接口；具体供应商只需实现这些属性和 complete。"""

    name: str
    model: str

    def complete(
        self,
        messages: list[Message],
        tools: list[dict] | None = None,
        max_tokens: int | None = None,
    ) -> LLMResponse: ...
