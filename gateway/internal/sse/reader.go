// 本文件解析 SSE 数据行、事件边界及 OpenAI 兼容的结束标记。
package sse

import (
	"bufio"
	"io"
	"strings"
)

const maxLineBytes = 4 << 20

// Event 保存从 SSE 流解析出的 data 内容及流结束标志。
type Event struct {
	Data string
	Done bool
}

// Reader 按 SSE 事件边界从输入流读取消息。
type Reader struct {
	scanner  *bufio.Scanner
	data     []string
	finished bool
}

// NewReader 创建 SSE reader，并将最大行长限制为 4 MiB。
func NewReader(source io.Reader) *Reader {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	return &Reader{scanner: scanner}
}

// Next 读取下一个 data 事件；到达流尾或 [DONE] 后返回 io.EOF 或 Done 事件。
func (r *Reader) Next() (Event, error) {
	if r.finished {
		return Event{}, io.EOF
	}
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if event, ok := r.flushEvent(); ok {
				return event, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasValue := strings.Cut(line, ":")
		if !hasValue {
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if field == "data" {
			// SSE 允许一个事件包含多个 data 行，事件边界到空行时再合并。
			r.data = append(r.data, value)
		}
	}
	r.finished = true
	if err := r.scanner.Err(); err != nil {
		return Event{}, err
	}
	if event, ok := r.flushEvent(); ok {
		return event, nil
	}
	return Event{}, io.EOF
}

func (r *Reader) flushEvent() (Event, bool) {
	if len(r.data) == 0 {
		return Event{}, false
	}
	data := strings.Join(r.data, "\n")
	r.data = nil
	if data == "[DONE]" {
		// 豆包/OpenAI 兼容协议用 [DONE] 表示流正常结束。
		r.finished = true
		return Event{Data: data, Done: true}, true
	}
	return Event{Data: data}, true
}
