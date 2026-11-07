"""工具注册表：schema 由 pydantic 生成，参数先校验再执行。"""

from __future__ import annotations

import json
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from pydantic import BaseModel, ValidationError


class ToolDenied(Exception):
    """越权或白名单外的请求：不是执行失败，而是明确拒绝。"""


@dataclass
class ToolResult:
    ok: bool
    content: str
    elapsed_ms: int = 0


@dataclass
class Tool:
    name: str
    description: str
    args_model: type[BaseModel]
    func: Callable[[Any], str]


@dataclass
class ToolRegistry:
    root: Path
    tools: dict[str, Tool] = field(default_factory=dict)

    def register(
        self, name: str, description: str, args_model: type[BaseModel]
    ) -> Callable[[Callable[[Any], str]], Callable[[Any], str]]:
        def deco(fn: Callable[[Any], str]) -> Callable[[Any], str]:
            self.tools[name] = Tool(name, description, args_model, fn)
            return fn

        return deco

    def schemas(self) -> list[dict]:
        out = []
        for t in self.tools.values():
            out.append(
                {
                    "type": "function",
                    "function": {
                        "name": t.name,
                        "description": t.description,
                        "parameters": t.args_model.model_json_schema(),
                    },
                }
            )
        return out

    def has(self, name: str) -> bool:
        return name in self.tools

    def invoke(self, name: str, raw_args: str) -> ToolResult:
        t0 = time.perf_counter()
        tool = self.tools.get(name)
        if tool is None:
            return ToolResult(False, f"未注册的工具：{name}")
        try:
            payload = json.loads(raw_args or "{}")
        except json.JSONDecodeError as exc:
            return ToolResult(False, f"参数不是合法 JSON：{exc}")
        try:
            args = tool.args_model.model_validate(payload)
        except ValidationError as exc:
            return ToolResult(False, f"参数校验失败：{exc.errors()[:3]}")
        try:
            content = tool.func(args)
        except ToolDenied as exc:
            return ToolResult(False, f"拒绝执行：{exc}")
        except Exception as exc:  # noqa: BLE001 工具内异常一律转成模型可读的错误
            return ToolResult(False, f"执行失败：{type(exc).__name__}: {exc}")
        return ToolResult(True, content, int((time.perf_counter() - t0) * 1000))
