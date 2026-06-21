package sseparse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"iter"
	"strings"
)

type Event struct {
	Event string
	Data  string
	ID    string
	Retry string
}

func Parse(reader io.Reader) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var event Event
		var data bytes.Buffer
		flush := func() bool {
			if event.Event == "" && event.ID == "" && event.Retry == "" && data.Len() == 0 {
				return true
			}
			event.Data = strings.TrimSuffix(data.String(), "\n")
			keepGoing := yield(event, nil)
			event = Event{}
			data.Reset()
			return keepGoing
		}

		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if line == "" {
				if !flush() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}

			field, value, ok := strings.Cut(line, ":")
			if !ok {
				field = line
				value = ""
			} else {
				value = strings.TrimPrefix(value, " ")
			}

			switch field {
			case "event":
				event.Event = value
			case "data":
				data.WriteString(value)
				data.WriteByte('\n')
			case "id":
				event.ID = value
			case "retry":
				event.Retry = value
			default:
				continue
			}
		}
		if err := scanner.Err(); err != nil {
			yield(Event{}, fmt.Errorf("parse sse: %w", err))
			return
		}
		flush()
	}
}
