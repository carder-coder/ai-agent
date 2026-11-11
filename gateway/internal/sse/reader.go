package sse

import (
	"bufio"
	"io"
	"strings"
)

const maxLineBytes = 4 << 20

type Event struct {
	Data string
	Done bool
}

type Reader struct {
	scanner  *bufio.Scanner
	data     []string
	finished bool
}

func NewReader(source io.Reader) *Reader {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	return &Reader{scanner: scanner}
}

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
		r.finished = true
		return Event{Data: data, Done: true}, true
	}
	return Event{Data: data}, true
}
