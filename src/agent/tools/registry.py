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
    """工具统一返回值；ok 区分执行结果与错误文本，elapsed_ms 用于 trace。"""

    ok: bool
    content: str
    elapsed_ms: int = 0


@dataclass
class Tool:
    """工具定义：模型可见的描述/schema 与实际执行函数绑定在一起。"""

    name: str
    description: str
    args_model: type[BaseModel]
    func: Callable[[Any], str]


@dataclass
class ToolRegistry:
    """管理工具定义，并集中处理参数解析、验证、执行和错误归一化。"""

    root: Path
    tools: dict[str, Tool] = field(default_factory=dict)

    def register(
        self, name: str, description: str, args_model: type[BaseModel]
    ) -> Callable[[Callable[[Any], str]], Callable[[Any], str]]:
        """以装饰器形式注册工具函数，参数模型同时作为校验规则和 schema 来源。"""
        def deco(fn: Callable[[Any], str]) -> Callable[[Any], str]:
            self.tools[name] = Tool(name, description, args_model, fn)
            return fn

        return deco

    def schemas(self) -> list[dict]:
        """生成模型 API 所需的 function-tool 描述，不暴露 Python 函数本身。"""
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
        """按固定顺序查找工具、解码 JSON、校验参数，再执行并包装异常。"""
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
            # 权限拒绝与执行错误都转成显式失败结果，供循环写 trace 并反馈模型。
            content = tool.func(args)
        except ToolDenied as exc:
            return ToolResult(False, f"拒绝执行：{exc}")
        except Exception as exc:  # noqa: BLE001 工具内异常一律转成模型可读的错误
            return ToolResult(False, f"执行失败：{type(exc).__name__}: {exc}")
        return ToolResult(True, content, int((time.perf_counter() - t0) * 1000))
