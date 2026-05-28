package opencode

import (
	"bufio"
	"context"
	"io"
	"strings"
)

// SSEEvent — event from a text/event-stream.
//
// Spec: each event is a block of lines, separated by a blank line.
// Fields: `event:`, `id:`, `data:` (data may repeat, joined with \n).
// Lines starting with `:` — comments (e.g. heartbeat in opencode).
type SSEEvent struct {
	ID   string
	Type string
	Data []byte
}

// readSSE parses the stream and sends events to out until r is closed or ctx cancelled.
// The channel is closed on exit.
func readSSE(ctx context.Context, r io.Reader, out chan<- SSEEvent) {
	defer close(out)
	reader := bufio.NewReader(r)

	var ev SSEEvent
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 && ev.ID == "" && ev.Type == "" {
			return
		}
		ev.Data = []byte(data.String())
		select {
		case out <- ev:
		case <-ctx.Done():
		}
		ev = SSEEvent{}
		data.Reset()
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if line != "" {
				processSSELine(line, &ev, &data)
			}
			flush()
			return
		}
		processSSELine(line, &ev, &data)
		// blank line (after stripping \r\n) — event separator
		stripped := strings.TrimRight(line, "\r\n")
		if stripped == "" {
			flush()
		}
	}
}

func processSSELine(line string, ev *SSEEvent, data *strings.Builder) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	if line[0] == ':' { // comment (heartbeat)
		return
	}
	field := line
	value := ""
	if i := strings.Index(line, ":"); i >= 0 {
		field = line[:i]
		value = strings.TrimPrefix(line[i+1:], " ")
	}
	switch field {
	case "event":
		ev.Type = value
	case "id":
		ev.ID = value
	case "data":
		if data.Len() > 0 {
			data.WriteByte('\n')
		}
		data.WriteString(value)
	}
}
