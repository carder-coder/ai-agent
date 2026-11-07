"""调用链落盘：一行一个事件，附带用量与成本估算。"""

from __future__ import annotations

import json
import os
import uuid
from datetime import datetime
from pathlib import Path

from ..llm.types import Usage

PRICE_IN = float(os.environ.get("ARK_PRICE_IN", "6"))
PRICE_OUT = float(os.environ.get("ARK_PRICE_OUT", "30"))
PRICE_CACHED = float(os.environ.get("ARK_PRICE_CACHED", "1.2"))


def cost_yuan(usage: Usage) -> float:
    """按元/百万 token 折算单次调用成本。"""
    billed_in = max(usage.input_tokens - usage.cached_tokens, 0)
    return (
        billed_in * PRICE_IN + usage.cached_tokens * PRICE_CACHED + usage.output_tokens * PRICE_OUT
    ) / 1_000_000


class Tracer:
    def __init__(self, path: Path | None = None, run_id: str | None = None) -> None:
        self.run_id = run_id or datetime.now().strftime("%Y%m%d-%H%M%S-") + uuid.uuid4().hex[:6]
        self.path = path or Path("traces") / f"{self.run_id}.jsonl"
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = self.path.open("w", encoding="utf-8", newline="\n")
        self.total_usage = Usage()
        self.total_cost = 0.0
        self.event("run_start", run_id=self.run_id)

    def event(self, kind: str, **payload) -> dict:
        rec = {"ts": datetime.now().isoformat(timespec="milliseconds"), "kind": kind, **payload}
        self._fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
        self._fh.flush()
        return rec

    def record_llm(self, step: int, model: str, response) -> None:
        self.total_usage.add(response.usage)
        c = cost_yuan(response.usage)
        self.total_cost += c
        self.event(
            "llm",
            step=step,
            model=model,
            first_byte_ms=response.first_byte_ms,
            latency_ms=response.latency_ms,
            finish_reason=response.finish_reason,
            usage={
                "in": response.usage.input_tokens,
                "out": response.usage.output_tokens,
                "total": response.usage.total_tokens,
            },
            cost_yuan=round(c, 6),
            tool_calls=[c2.name for c2 in response.message.tool_calls],
        )

    def record_tool(self, step: int, name: str, ok: bool, elapsed_ms: int, content: str) -> None:
        self.event(
            "tool",
            step=step,
            name=name,
            ok=ok,
            elapsed_ms=elapsed_ms,
            preview=content[:300],
        )

    def close(self, **payload) -> None:
        self.event(
            "run_end",
            total_cost_yuan=round(self.total_cost, 6),
            usage={
                "in": self.total_usage.input_tokens,
                "out": self.total_usage.output_tokens,
                "total": self.total_usage.total_tokens,
            },
            **payload,
        )
        self._fh.close()
