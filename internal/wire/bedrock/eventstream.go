package bedrock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// The binary framing `application/vnd.amazon.eventstream` uses on
// converse-stream. Each message is:
//
//	prelude    total length (4) | headers length (4) | prelude CRC (4)
//	headers    name length (1) | name | value type (1) | value
//	payload
//	message CRC (4)   over everything before it
//
// Both CRCs are CRC-32 (IEEE). A header value of type 7 is a string with a
// two-byte length; the routing headers dorang reads — :message-type,
// :event-type, :exception-type, :content-type — are all of that type.

// maxEventStreamMessage bounds one message. A payload here is one JSON event
// of a Converse stream, never more than a few kilobytes; the bound is against
// a corrupt length field asking for gigabytes.
const maxEventStreamMessage = 16 << 20

// ErrEventStreamFrame is a message that does not parse or does not check.
var ErrEventStreamFrame = errors.New("bedrock: event stream frame")

// EventMessage is one decoded message.
type EventMessage struct {
	Headers map[string]string
	Payload []byte
}

// MessageType, EventType and ExceptionType read the routing headers.
func (m *EventMessage) MessageType() string   { return m.Headers[":message-type"] }
func (m *EventMessage) EventType() string     { return m.Headers[":event-type"] }
func (m *EventMessage) ExceptionType() string { return m.Headers[":exception-type"] }

// EventStreamReader reads messages from a stream.
type EventStreamReader struct {
	r io.Reader
}

// NewEventStreamReader wraps r.
func NewEventStreamReader(r io.Reader) *EventStreamReader { return &EventStreamReader{r: r} }

// Next reads one message, or io.EOF at a clean end.
func (e *EventStreamReader) Next() (*EventMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(e.r, prelude[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("%w: cut off inside the prelude", ErrEventStreamFrame)
		}
		return nil, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	hlen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[0:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return nil, fmt.Errorf("%w: prelude checksum", ErrEventStreamFrame)
	}
	if total < 16 || total > maxEventStreamMessage || hlen > total-16 {
		return nil, fmt.Errorf("%w: length %d/%d", ErrEventStreamFrame, total, hlen)
	}
	rest := make([]byte, total-12)
	if _, err := io.ReadFull(e.r, rest); err != nil {
		return nil, fmt.Errorf("%w: cut off inside the message", ErrEventStreamFrame)
	}
	body := rest[:len(rest)-4]
	want := binary.BigEndian.Uint32(rest[len(rest)-4:])
	sum := crc32.ChecksumIEEE(prelude[:])
	sum = crc32.Update(sum, crc32.IEEETable, body)
	if sum != want {
		return nil, fmt.Errorf("%w: message checksum", ErrEventStreamFrame)
	}
	headers, err := decodeHeaders(body[:hlen])
	if err != nil {
		return nil, err
	}
	payload := make([]byte, len(body)-int(hlen))
	copy(payload, body[hlen:])
	return &EventMessage{Headers: headers, Payload: payload}, nil
}

func decodeHeaders(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(b) > 0 {
		nlen := int(b[0])
		if len(b) < 1+nlen+1 {
			return nil, fmt.Errorf("%w: header name", ErrEventStreamFrame)
		}
		name := string(b[1 : 1+nlen])
		typ := b[1+nlen]
		b = b[2+nlen:]
		switch typ {
		case 7: // string
			if len(b) < 2 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			vlen := int(binary.BigEndian.Uint16(b[0:2]))
			if len(b) < 2+vlen {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			out[name] = string(b[2 : 2+vlen])
			b = b[2+vlen:]
		case 0, 1: // bool true / false
		case 2: // byte
			if len(b) < 1 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			b = b[1:]
		case 3: // int16
			if len(b) < 2 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			b = b[2:]
		case 4: // int32
			if len(b) < 4 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			b = b[4:]
		case 5, 8: // int64, timestamp
			if len(b) < 8 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			b = b[8:]
		case 6: // byte array
			if len(b) < 2 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			vlen := int(binary.BigEndian.Uint16(b[0:2]))
			if len(b) < 2+vlen {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			b = b[2+vlen:]
		case 9: // uuid
			if len(b) < 16 {
				return nil, fmt.Errorf("%w: header value", ErrEventStreamFrame)
			}
			b = b[16:]
		default:
			return nil, fmt.Errorf("%w: header type %d", ErrEventStreamFrame, typ)
		}
	}
	return out, nil
}

// EncodeEventMessage renders one message. It exists for tests and for any
// fake that must speak the framing; the gateway never writes this format.
func EncodeEventMessage(headers map[string]string, payload []byte) []byte {
	var hb []byte
	for name, value := range headers {
		hb = append(hb, byte(len(name)))
		hb = append(hb, name...)
		hb = append(hb, 7)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(value)))
		hb = append(hb, l[:]...)
		hb = append(hb, value...)
	}
	total := 12 + len(hb) + len(payload) + 4
	out := make([]byte, 0, total)
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], uint32(total))
	out = append(out, u[:]...)
	binary.BigEndian.PutUint32(u[:], uint32(len(hb)))
	out = append(out, u[:]...)
	binary.BigEndian.PutUint32(u[:], crc32.ChecksumIEEE(out[:8]))
	out = append(out, u[:]...)
	out = append(out, hb...)
	out = append(out, payload...)
	binary.BigEndian.PutUint32(u[:], crc32.ChecksumIEEE(out))
	out = append(out, u[:]...)
	return out
}
