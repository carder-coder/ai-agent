"""豆包（火山方舟）适配：流式调用、usage 取真值、错误归类。"""

from __future__ import annotations

import os
import time

from openai import APITimeoutError, OpenAI, RateLimitError

from .types import (
    LLMResponse,
    Message,
    ProviderError,
    ProviderRateLimited,
    ProviderTimeout,
    ToolCall,
    Usage,
)

DEFAULT_BASE_URL = "https://ark.cn-beijing.volces.com/api/v3"


class DoubaoClient:
    name = "doubao"

    def __init__(
        self,
        model: str | None = None,
        api_key: str | None = None,
        base_url: str | None = None,
        timeout: float = 60.0,
    ) -> None:
        self.api_key = api_key or os.environ.get("ARK_API_KEY", "")
        if not self.api_key:
            raise ProviderError("缺少 ARK_API_KEY，请复制 .env.example 为 .env 后填写")
        self.model = model or os.environ.get("ARK_MODEL", "")
        if not self.model:
            raise ProviderError("缺少 ARK_MODEL，填入模型 ID 或 ep- 接入点 ID")
        self.base_url = base_url or os.environ.get("ARK_BASE_URL", DEFAULT_BASE_URL)
        self._client = OpenAI(api_key=self.api_key, base_url=self.base_url, timeout=timeout)

    def complete(self, messages: list[Message], tools: list[dict] | None = None) -> LLMResponse:
        t0 = time.perf_counter()
        first_byte_ms: int | None = None
        text_parts: list[str] = []
        calls: dict[int, dict] = {}
        usage = Usage()
        finish_reason = "stop"

        try:
            stream = self._client.chat.completions.create(
                model=self.model,
                messages=[m.to_openai() for m in messages],
                tools=tools or None,
                stream=True,
                stream_options={"include_usage": True},
            )
            for chunk in stream:
                if first_byte_ms is None and chunk.choices:
                    first_byte_ms = int((time.perf_counter() - t0) * 1000)
                if getattr(chunk, "usage", None):
                    usage = Usage(
                        input_tokens=chunk.usage.prompt_tokens or 0,
                        output_tokens=chunk.usage.completion_tokens or 0,
                        total_tokens=chunk.usage.total_tokens or 0,
                        cached_tokens=getattr(chunk.usage, "prompt_tokens_details", None)
                        and (chunk.usage.prompt_tokens_details.cached_tokens or 0)
                        or 0,
                    )
                if not chunk.choices:
                    continue
                choice = chunk.choices[0]
                if choice.finish_reason:
                    finish_reason = choice.finish_reason
                delta = choice.delta
                if delta.content:
                    text_parts.append(delta.content)
                for frag in delta.tool_calls or []:
                    slot = calls.setdefault(frag.index, {"id": "", "name": "", "args": []})
                    if frag.id:
                        slot["id"] = frag.id
                    if frag.function and frag.function.name:
                        slot["name"] = frag.function.name
                    if frag.function and frag.function.arguments:
                        slot["args"].append(frag.function.arguments)
        except APITimeoutError as exc:
            raise ProviderTimeout(f"上游超时：{exc}") from exc
        except RateLimitError as exc:
            raise ProviderRateLimited(f"被限流：{exc}") from exc
        except Exception as exc:  # noqa: BLE001 其余按可重试错误抛出，保留原始信息
            raise ProviderError(f"调用失败：{type(exc).__name__}: {exc}") from exc

        tool_calls = [
            ToolCall(id=v["id"] or f"call_{i}", name=v["name"], arguments="".join(v["args"]))
            for i, v in sorted(calls.items())
        ]
        message = Message(
            role="assistant",
            content="".join(text_parts) or None,
            tool_calls=tool_calls,
        )
        return LLMResponse(
            message=message,
            usage=usage,
            finish_reason=finish_reason,
            first_byte_ms=first_byte_ms,
            latency_ms=int((time.perf_counter() - t0) * 1000),
        )
