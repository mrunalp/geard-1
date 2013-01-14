package dbus

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
)

const protoVersion byte = 1

// Flags represents the possible flags of a DBus message.
type Flags byte

const (
	NoReplyExpected Flags = 1 << iota
	NoAutoStart
)

// Type represents the possible types of a DBus message.
type Type byte

const (
	TypeMethodCall Type = 1 + iota
	TypeMethodReply
	TypeError
	TypeSignal
	typeMax
)

// HeaderField represents the possible byte codes for the headers
// of a DBus message.
type HeaderField byte

const (
	FieldPath HeaderField = 1 + iota
	FieldInterface
	FieldMember
	FieldErrorName
	FieldReplySerial
	FieldDestination
	FieldSender
	FieldSignature
	FieldUnixFds
	fieldMax
)

type InvalidMessageError string

func (e InvalidMessageError) Error() string {
	return "invalid message: " + string(e)
}

var fieldTypes = map[HeaderField]reflect.Type{
	FieldPath:        objectPathType,
	FieldInterface:   stringType,
	FieldMember:      stringType,
	FieldErrorName:   stringType,
	FieldReplySerial: uint32Type,
	FieldDestination: stringType,
	FieldSender:      stringType,
	FieldSignature:   signatureType,
	FieldUnixFds:     uint32Type,
}

var requiredFields = map[Type][]HeaderField{
	TypeMethodCall:  []HeaderField{FieldPath, FieldMember},
	TypeMethodReply: []HeaderField{FieldReplySerial},
	TypeError:       []HeaderField{FieldErrorName, FieldReplySerial},
	TypeSignal:      []HeaderField{FieldPath, FieldInterface, FieldMember},
}

// Message represents a single DBus message.
type Message struct {
	// must be binary.BigEndian or binary.LittleEndian
	Order binary.ByteOrder

	Type
	Flags
	Serial  uint32
	Headers map[HeaderField]Variant
	Body    []byte
}

type header struct {
	HeaderField
	Variant
}

// DecodeMessage tries to decode a single message from the given reader.
// The byte order is figured out from the first byte. The possibly returned
// error may either be an error of the underlying reader or an
// InvalidMessageError.
func DecodeMessage(rd io.Reader) (message *Message, err error) {
	var order binary.ByteOrder
	var length uint32
	var proto byte
	var headers []header

	b := make([]byte, 1)
	_, err = rd.Read(b)
	if err != nil {
		return
	}
	switch b[0] {
	case 'l':
		order = binary.LittleEndian
	case 'B':
		order = binary.BigEndian
	default:
		return nil, InvalidMessageError("invalid byte order")
	}

	dec := NewDecoder(rd, order)
	dec.pos = 1

	message = new(Message)
	message.Order = order
	err = dec.DecodeMulti(&message.Type, &message.Flags, &proto, &length,
		&message.Serial, &headers)
	if err != nil {
		return nil, err
	}

	message.Headers = make(map[HeaderField]Variant)
	for _, v := range headers {
		message.Headers[v.HeaderField] = v.Variant
	}

	dec.align(8)
	message.Body = make([]byte, int(length))
	if length != 0 {
		_, err := rd.Read(message.Body)
		if err != nil {
			return nil, err
		}
	}

	if err = message.IsValid(); err != nil {
		return nil, err
	}

	return
}

// EncodeTo encodes and sends a message to the given writer. If the message is
// not valid or an error occurs when writing, an error is returned.
func (message *Message) EncodeTo(out io.Writer) error {
	if err := message.IsValid(); err != nil {
		return err
	}
	vs := make([]interface{}, 7)
	switch message.Order {
	case binary.LittleEndian:
		vs[0] = byte('l')
	case binary.BigEndian:
		vs[0] = byte('B')
	}
	vs[1] = message.Type
	vs[2] = message.Flags
	vs[3] = protoVersion
	vs[4] = uint32(len(message.Body))
	vs[5] = message.Serial
	headers := make([]header, 0)
	for k, v := range message.Headers {
		headers = append(headers, header{k, v})
	}
	vs[6] = headers
	buf := new(bytes.Buffer)
	enc := NewEncoder(buf, binary.LittleEndian)
	enc.EncodeMulti(vs...)
	enc.align(8)
	if len(message.Body) != 0 {
		buf.Write(message.Body)
	}
	if _, err := buf.WriteTo(out); err != nil {
		return err
	}
	return nil
}

// IsValid checks whether message is a valid message and returns an
// InvalidMessageError if it is not.
func (message *Message) IsValid() error {
	switch message.Order {
	case binary.LittleEndian, binary.BigEndian:
	default:
		return InvalidMessageError("invalid byte order")
	}
	if message.Flags & ^(NoAutoStart|NoReplyExpected) != 0 {
		return InvalidMessageError("invalid flags")
	}
	if message.Type == 0 || message.Type >= typeMax {
		return InvalidMessageError("invalid message type")
	}
	for k, v := range message.Headers {
		if k == 0 || k >= fieldMax {
			return InvalidMessageError("invalid header")
		}
		if reflect.TypeOf(v.value) != fieldTypes[k] {
			return InvalidMessageError("invalid type of header field")
		}
	}
	for _, v := range requiredFields[message.Type] {
		if _, ok := message.Headers[v]; !ok {
			return InvalidMessageError("missing required header")
		}
	}
	if len(message.Body) != 0 {
		if _, ok := message.Headers[FieldSignature]; !ok {
			return InvalidMessageError("missing signature")
		}
	}
	return nil
}