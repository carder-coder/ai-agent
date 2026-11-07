"""豆包（火山方舟）适配：流式调用、usage 取真值、错误归类、限流退避。"""

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
RETRY_DELAYS: tuple[float, ...] = (1.0, 2.0, 4.0)


class DoubaoClient:
    name = "doubao"

    def __init__(
        self,
        model: str | None = None,
        api_key: str | None = None,
        base_url: str | None = None,
        timeout: float = 60.0,
        max_retries: int = 3,
    ) -> None:
        self.api_key = api_key or os.environ.get("ARK_API_KEY", "")
        if not self.api_key:
            raise ProviderError("缺少 ARK_API_KEY，请复制 .env.example 为 .env 后填写")
        self.model = model or os.environ.get("ARK_MODEL", "")
        if not self.model:
            raise ProviderError("缺少 ARK_MODEL，填入模型 ID 或 ep- 接入点 ID")
        self.base_url = base_url or os.environ.get("ARK_BASE_URL", DEFAULT_BASE_URL)
        self.max_retries = max_retries
        # SDK 自带重试会和我们自己的退避叠加，这里关掉，退避只留一处
        self._client = OpenAI(
            api_key=self.api_key,
            base_url=self.base_url,
            timeout=timeout,
            max_retries=0,
        )
        self.last_attempts = 0
        self.last_retry_delays: list[float] = []

    def complete(self, messages: list[Message], tools: list[dict] | None = None) -> LLMResponse:
        """限流按 1、2、4 秒退避重试；超时与其他错误不重试，直接抛。"""
        self.last_attempts = 0
        self.last_retry_delays = []
        for attempt in range(1, self.max_retries + 2):
            self.last_attempts = attempt
            try:
                return self._stream_once(messages, tools, attempt)
            except ProviderRateLimited:
                if attempt > self.max_retries:
                    raise
                delay = RETRY_DELAYS[attempt - 1]
                self.last_retry_delays.append(delay)
                time.sleep(delay)
        raise ProviderError("退避重试用尽")

    def _stream_once(
        self, messages: list[Message], tools: list[dict] | None, attempt: int
    ) -> LLMResponse:
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
                    details = getattr(chunk.usage, "prompt_tokens_details", None)
                    usage = Usage(
                        input_tokens=chunk.usage.prompt_tokens or 0,
                        output_tokens=chunk.usage.completion_tokens or 0,
                        total_tokens=chunk.usage.total_tokens or 0,
                        cached_tokens=(details.cached_tokens or 0) if details else 0,
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
                        # 名字也可能被切成多片，拼接而不是覆盖
                        slot["name"] += frag.function.name
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
            attempts=attempt,
        )
