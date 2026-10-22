// Package eventstream writes AWS event-stream frames independently of service protocols.
package eventstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// Message contains typed headers and an opaque payload.
type Message = sdk.Message

// Stream is an ordered sequence of event-stream messages.
type Stream []Message

// WriteTo writes complete frames and flushes each frame to the client.
func (s *Stream) WriteTo(w io.Writer) (int64, error) {
	counter := &countWriter{writer: w}
	for _, message := range *s {
		if err := Encode(counter, message); err != nil {
			return counter.n, err
		}

		if response, ok := w.(http.ResponseWriter); ok {
			if err := http.NewResponseController(response).Flush(); err != nil &&
				!errors.Is(err, http.ErrNotSupported) {
				return counter.n, err
			}
		}
	}

	return counter.n, nil
}

type countWriter struct {
	writer io.Writer
	n      int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.n += int64(n)
	return n, err
}

// Event constructs a JSON event or exception with the standard typed headers.
func Event(kind, name string, payload []byte) Message {
	header := ":event-type"
	if kind == "exception" {
		header = ":exception-type"
	}

	return Message{Headers: sdk.Headers{
		{Name: ":message-type", Value: sdk.StringValue(kind)},
		{Name: header, Value: sdk.StringValue(name)},
		{Name: ":content-type", Value: sdk.StringValue("application/json")},
	}, Payload: payload}
}

// Encode writes a frame with a checksummed prelude and message. Header values
// use the SDK's typed representation so callers can share decoded messages.
func Encode(w io.Writer, message Message) error {
	headers := []byte{}
	for _, header := range message.Headers {
		if header.Name == "" || len(header.Name) > 255 || header.Value == nil {
			return fmt.Errorf("invalid event-stream header")
		}

		// #nosec G115 -- bounded length or signed wire bits.
		headers = append(headers, byte(len(header.Name)))
		headers = append(headers, header.Name...)
		if uuid, ok := header.Value.(sdk.UUIDValue); ok {
			headers = append(headers, 9)
			headers = append(headers, uuid[:]...)
			continue
		}

		switch value := header.Value.Get().(type) {
		case bool:
			if value {
				headers = append(headers, 0)
			} else {
				headers = append(headers, 1)
			}
		case int8:
			// #nosec G115 -- signed wire bits.
			headers = append(headers, 2, byte(value))
		case int16:
			headers = append(headers, 3)
			// #nosec G115 -- bounded length or signed wire bits.
			headers = binary.BigEndian.AppendUint16(headers, uint16(value))
		case int32:
			headers = append(headers, 4)
			// #nosec G115 -- bounded length or signed wire bits.
			headers = binary.BigEndian.AppendUint32(headers, uint32(value))
		case int64:
			headers = append(headers, 5)
			// #nosec G115 -- bounded length or signed wire bits.
			headers = binary.BigEndian.AppendUint64(headers, uint64(value))
		case []byte:
			if len(value) > 65535 {
				return fmt.Errorf("event-stream header value too long")
			}

			headers = append(headers, 6)
			// #nosec G115 -- bounded length or signed wire bits.
			headers = binary.BigEndian.AppendUint16(headers, uint16(len(value)))
			headers = append(headers, value...)
		case string:
			if len(value) > 65535 {
				return fmt.Errorf("event-stream header value too long")
			}

			headers = append(headers, 7)
			// #nosec G115 -- bounded length or signed wire bits.
			headers = binary.BigEndian.AppendUint16(headers, uint16(len(value)))
			headers = append(headers, value...)
		case time.Time:
			headers = append(headers, 8)
			headers = binary.BigEndian.AppendUint64(headers, uint64(value.UnixMilli()))
		default:
			return fmt.Errorf("unsupported event-stream header value %T", value)
		}
	}

	if len(headers) > 128*1024 || len(message.Payload) > 24*1024*1024 {
		return fmt.Errorf("event-stream message exceeds size limit")
	}

	frame := make([]byte, 0, 16+len(headers)+len(message.Payload))
	// #nosec G115 -- bounded length or signed wire bits.
	frame = binary.BigEndian.AppendUint32(frame, uint32(16+len(headers)+len(message.Payload)))
	// #nosec G115 -- bounded length or signed wire bits.
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(headers)))
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	frame = append(frame, headers...)
	frame = append(frame, message.Payload...)
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	_, err := w.Write(frame)
	return err
}
