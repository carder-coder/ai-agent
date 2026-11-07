"""消息、工具调用、用量与响应对象：全链路只认这几个结构。"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class ToolCall:
    id: str
    name: str
    arguments: str  # 模型返回的原始 JSON 字符串，未解析


@dataclass
class Message:
    role: str
    content: str | None = None
    tool_calls: list[ToolCall] = field(default_factory=list)
    tool_call_id: str | None = None
    name: str | None = None

    def to_openai(self) -> dict:
        if self.role == "tool":
            return {
                "role": "tool",
                "tool_call_id": self.tool_call_id,
                "content": self.content or "",
            }
        if self.role == "assistant" and self.tool_calls:
            return {
                "role": "assistant",
                "content": self.content or "",
                "tool_calls": [
                    {
                        "id": c.id,
                        "type": "function",
                        "function": {"name": c.name, "arguments": c.arguments},
                    }
                    for c in self.tool_calls
                ],
            }
        return {"role": self.role, "content": self.content or ""}


@dataclass
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    cached_tokens: int = 0

    def add(self, other: Usage) -> None:
        self.input_tokens += other.input_tokens
        self.output_tokens += other.output_tokens
        self.total_tokens += other.total_tokens
        self.cached_tokens += other.cached_tokens


@dataclass
class LLMResponse:
    message: Message
    usage: Usage = field(default_factory=Usage)
    finish_reason: str = "stop"
    first_byte_ms: int | None = None
    latency_ms: int | None = None
    attempts: int = 1  # 本次调用实际发了几次请求（含退避重试）


class ProviderError(RuntimeError):
    """模型侧可归类的错误，循环只按类型决定停还是重试。"""


class ProviderTimeout(ProviderError):
    pass


class ProviderRateLimited(ProviderError):
    pass
