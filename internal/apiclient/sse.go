package apiclient

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	sseMaxLineBytes  = 1 << 20
	sseMaxFrameBytes = 4 << 20
)

var (
	errSSELineTooLong   = errors.New("SSE line exceeds 1 MiB")
	errSSEFrameTooLarge = errors.New("SSE frame exceeds 4 MiB")
)

// sseFrame represents one dispatch; id and retry are optional state updates.
type sseFrame struct {
	id       string
	event    string
	data     string
	retry    time.Duration
	hasID    bool
	hasRetry bool
	hasData  bool
}

// readSSEFrame reads one blank-line-terminated frame. An incomplete frame at
// EOF is discarded. Frames without data are returned with hasData false.
func readSSEFrame(reader *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	var dataLines []string
	frameBytes := 0
	for {
		line, size, err := readSSELine(reader)
		if err != nil {
			return sseFrame{}, err
		}
		frameBytes += size
		if frameBytes > sseMaxFrameBytes {
			return sseFrame{}, errSSEFrameTooLarge
		}
		if line == "" {
			frame.data = strings.Join(dataLines, "\n")
			frame.hasData = len(dataLines) > 0
			return frame, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			frame.event = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				frame.id = value
				frame.hasID = true
			}
		case "retry":
			if isASCIIDigits(value) {
				if millis, parseErr := strconv.ParseUint(value, 10, 64); parseErr == nil {
					maxMillis := uint64(time.Duration(1<<63-1) / time.Millisecond)
					if millis > maxMillis {
						millis = maxMillis
					}
					frame.retry = time.Duration(millis) * time.Millisecond
					frame.hasRetry = true
				}
			}
		}
	}
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func readSSELine(reader *bufio.Reader) (string, int, error) {
	line := make([]byte, 0, 128)
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > sseMaxLineBytes+2 {
			return "", 0, errSSELineTooLong
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", 0, io.EOF
			}
			return "", 0, err
		}
		line = line[:len(line)-1]
		rawSize := len(line) + 1
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) > sseMaxLineBytes {
			return "", 0, errSSELineTooLong
		}
		return string(line), rawSize, nil
	}
}
