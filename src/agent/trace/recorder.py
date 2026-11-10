"""调用链落盘：一行一个事件，附带用量与成本估算。"""

from __future__ import annotations

import json
import os
import uuid
from datetime import datetime
from pathlib import Path

from ..llm.types import Usage


def prices() -> tuple[float, float, float]:
    """每次调用都读环境变量，避免模块导入早于 .env 加载。"""
    return (
        float(os.environ.get("ARK_PRICE_IN", "0.8")),
        float(os.environ.get("ARK_PRICE_OUT", "3.2")),
        float(os.environ.get("ARK_PRICE_CACHED", "0.016")),
    )


def cost_yuan(usage: Usage) -> float:
    """按元/百万 token 折算单次调用成本：输入、输出、缓存命中三档计价。"""
    price_in, price_out, price_cached = prices()
    billed_in = max(usage.input_tokens - usage.cached_tokens, 0)
    return (
        billed_in * price_in + usage.cached_tokens * price_cached + usage.output_tokens * price_out
    ) / 1_000_000


class Tracer:
    """将单次运行的事件追加为 JSONL，并维护本次累计用量与成本。"""

    def __init__(
        self,
        path: Path | None = None,
        run_id: str | None = None,
        append: bool = False,
        resumed_from: str | None = None,
        total_usage: Usage | None = None,
        total_cost: float = 0.0,
        task: str | None = None,
    ) -> None:
        self.run_id = run_id or datetime.now().strftime("%Y%m%d-%H%M%S-") + uuid.uuid4().hex[:6]
        self.path = path or Path("traces") / f"{self.run_id}.jsonl"
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = self.path.open("a" if append else "w", encoding="utf-8", newline="\n")
        self.total_usage = total_usage or Usage()
        self.total_cost = total_cost
        self.event("run_start", run_id=self.run_id, resumed_from=resumed_from, task=task)

    def event(self, kind: str, **payload) -> dict:
        """写入并立即 flush 单条事件，降低运行中断时丢失 trace 的风险。"""
        rec = {"ts": datetime.now().isoformat(timespec="milliseconds"), "kind": kind, **payload}
        self._fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
        self._fh.flush()
        return rec

    def record_llm(self, step: int, model: str, response, context: dict | None = None) -> None:
        """累计真实 usage，并记录延迟、重试、工具名、上下文裁剪与当前费率下的成本。"""
        self.total_usage.add(response.usage)
        c = cost_yuan(response.usage)
        self.total_cost += c
        payload = {
            "step": step,
            "model": model,
            "first_byte_ms": response.first_byte_ms,
            "latency_ms": response.latency_ms,
            "finish_reason": response.finish_reason,
            "usage": {
                "in": response.usage.input_tokens,
                "out": response.usage.output_tokens,
                "total": response.usage.total_tokens,
                "cached": response.usage.cached_tokens,
            },
            "attempts": getattr(response, "attempts", 1),
            "cost_yuan": round(c, 6),
            "tool_calls": [c2.name for c2 in response.message.tool_calls],
        }
        if context is not None:
            payload["context"] = context
        self.event("llm", **payload)

    def record_tool(
        self,
        step: int,
        name: str,
        ok: bool,
        elapsed_ms: int,
        content: str,
        arguments: str | None = None,
    ) -> None:
        """记录工具元数据和有限预览，避免把完整工具输出重复写入 trace。"""
        self.event(
            "tool",
            step=step,
            name=name,
            arguments=arguments,
            ok=ok,
            elapsed_ms=elapsed_ms,
            preview=content[:300],
        )

    def close(self, **payload) -> None:
        """写入汇总事件并关闭文件；调用方应在一次运行结束时调用。"""
        self.event(
            "run_end",
            total_cost_yuan=round(self.total_cost, 6),
            usage={
                "in": self.total_usage.input_tokens,
                "out": self.total_usage.output_tokens,
                "total": self.total_usage.total_tokens,
                "cached": self.total_usage.cached_tokens,
            },
            **payload,
        )
        self._fh.close()

    def abort(self, **payload) -> None:
        """记录可恢复的中断并关闭文件，不写成任务正常结束。"""
        self.event("run_interrupted", **payload)
        self._fh.close()
