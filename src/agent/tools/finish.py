"""finish：唯一的收尾工具，循环见到它就停止并输出结论。"""

from __future__ import annotations

from pydantic import BaseModel, Field


class FinishArgs(BaseModel):
    """最终答案的唯一参数结构，要求模型把结论整理成单段摘要。"""

    summary: str = Field(description="给用户的最终结论，含关键数字与依据")


def finish(args: FinishArgs) -> str:
    """提供可识别的完成标记；循环会将调用本身视为停止信号。"""
    return f"FINISHED: {args.summary}"
