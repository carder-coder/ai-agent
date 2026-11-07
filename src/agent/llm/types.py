"""消息、工具调用、用量与响应对象：全链路只认这几个结构。"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class ToolCall:
    """模型要求执行的工具调用；arguments 保留原始 JSON，校验留给工具注册表。"""

    id: str
    name: str
    arguments: str  # 模型返回的原始 JSON 字符串，未解析


@dataclass
class Message:
    """内部统一消息结构，在模型适配边界转换成 OpenAI 兼容格式。"""

    role: str
    content: str | None = None
    tool_calls: list[ToolCall] = field(default_factory=list)
    tool_call_id: str | None = None
    name: str | None = None

    def to_openai(self) -> dict:
        """按消息角色序列化，保留工具调用 ID 以便供应商关联工具结果。"""
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
    """一次或多次模型调用的 token 用量；cached_tokens 是 input_tokens 的子集。"""

    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    cached_tokens: int = 0

    def add(self, other: Usage) -> None:
        """将一次响应的用量累加到当前任务总量。"""
        self.input_tokens += other.input_tokens
        self.output_tokens += other.output_tokens
        self.total_tokens += other.total_tokens
        self.cached_tokens += other.cached_tokens


@dataclass
class LLMResponse:
    """模型适配层向循环返回的完整结果及可观测指标。"""

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
