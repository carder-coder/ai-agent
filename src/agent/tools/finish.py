"""finish：唯一的收尾工具，循环见到它就停止并输出结论。"""

from __future__ import annotations

from pydantic import BaseModel, Field


class FinishArgs(BaseModel):
    summary: str = Field(description="给用户的最终结论，含关键数字与依据")


def finish(args: FinishArgs) -> str:
    return f"FINISHED: {args.summary}"
