// Package rpcv2cbor implements the Smithy RPC v2 CBOR protocol: requests are
// POSTs to /service/<service>/operation/<operation> with a CBOR body and a
// "smithy-protocol: rpc-v2-cbor" header. Generated types describe the body
// through their JSON member names; timestamps carry a `cbor:"timestamp"` tag
// and travel as CBOR tag 1 (epoch seconds).
package rpcv2cbor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

const (
	// ContentType is the media type of requests and responses.
	ContentType = "application/cbor"
	// Header names the protocol on requests and responses.
	Header = "Smithy-Protocol"
	// Protocol is the Header's value.
	Protocol = "rpc-v2-cbor"
	// maxDepth bounds nesting in decoded documents.
	maxDepth = 64
)

// Decode reads a CBOR map into input, a generated input type. Members the
// type does not declare fail instead of being ignored. An empty body is an
// empty input.
func Decode(body io.Reader, input any) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return awserror.Validation("Cannot read request body: %v", err)
	}

	if len(data) == 0 {
		return nil
	}

	d := &decoder{data: data}
	v, err := d.value(0)
	if err != nil || d.pos != len(data) {
		return awserror.New("SerializationException", "The request body is not one CBOR value")
	}

	if _, ok := v.(map[string]any); !ok {
		return awserror.New("SerializationException", "Expected a CBOR map")
	}

	// Blobs become base64 text and timestamps numbers, as the JSON members of
	// generated types expect.
	doc, err := json.Marshal(v)
	if err != nil {
		return awserror.New("SerializationException", "The request body cannot be decoded")
	}

	decoder := json.NewDecoder(bytes.NewReader(doc))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(input); err != nil {
		return awserror.Validation("Invalid or unsupported request field: %v", err)
	}

	return nil
}

type decoder struct {
	data []byte
	pos  int
}

var errShort = errors.New("truncated CBOR")

func (d *decoder) byte() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, errShort
	}

	b := d.data[d.pos]
	d.pos++
	return b, nil
}

func (d *decoder) take(n uint64) ([]byte, error) {
	if n > uint64(len(d.data)-d.pos) { // #nosec G115 -- pos never passes len(data).
		return nil, errShort
	}

	size := int(n) // #nosec G115 -- n is at most the bytes left.
	b := d.data[d.pos : d.pos+size]
	d.pos += size
	return b, nil
}

// argument reads a head's argument; indefinite reports additional info 31.
func (d *decoder) argument(info byte) (n uint64, indefinite bool, err error) {
	switch {
	case info < 24:
		return uint64(info), false, nil
	case info == 31:
		return 0, true, nil
	case info > 27:
		return 0, false, fmt.Errorf("invalid CBOR additional information %d", info)
	}

	b, err := d.take(1 << (info - 24))
	if err != nil {
		return 0, false, err
	}

	for _, c := range b {
		n = n<<8 | uint64(c)
	}

	return n, false, nil
}

const breakByte = 0xff

//nolint:gocyclo,cyclop // One case per CBOR major type.
func (d *decoder) value(depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("CBOR nested too deeply")
	}

	head, err := d.byte()
	if err != nil {
		return nil, err
	}

	major, info := head>>5, head&0x1f
	if major == 7 {
		return d.simple(info)
	}

	n, indefinite, err := d.argument(info)
	if err != nil {
		return nil, err
	}

	switch major {
	case 0:
		return float64(n), nil
	case 1:
		return -1 - float64(n), nil
	case 2, 3:
		var b []byte
		if b, err = d.chunks(major, n, indefinite); err != nil {
			return nil, err
		}

		if major == 3 {
			return string(b), nil
		}

		return b, nil
	case 4:
		list := []any{}
		for i := uint64(0); indefinite || i < n; i++ {
			if indefinite && d.pos < len(d.data) && d.data[d.pos] == breakByte {
				d.pos++
				break
			}

			item, itemErr := d.value(depth + 1)
			if itemErr != nil {
				return nil, itemErr
			}

			list = append(list, item)
		}

		return list, nil
	case 5:
		m := map[string]any{}
		for i := uint64(0); indefinite || i < n; i++ {
			if indefinite && d.pos < len(d.data) && d.data[d.pos] == breakByte {
				d.pos++
				break
			}

			key, keyErr := d.value(depth + 1)
			if keyErr != nil {
				return nil, keyErr
			}

			name, ok := key.(string)
			if !ok {
				return nil, errors.New("CBOR map keys must be text")
			}

			if m[name], err = d.value(depth + 1); err != nil {
				return nil, err
			}
		}

		return m, nil
	case 6:
		// Tag 1 is an epoch timestamp; other tags keep their content.
		return d.value(depth + 1)
	}

	return nil, fmt.Errorf("unknown CBOR major type %d", major)
}

// chunks reads a byte or text string, joining an indefinite string's chunks.
func (d *decoder) chunks(major byte, n uint64, indefinite bool) ([]byte, error) {
	if !indefinite {
		return d.take(n)
	}

	var out []byte
	for {
		head, err := d.byte()
		if err != nil {
			return nil, err
		}

		if head == breakByte {
			return out, nil
		}

		if head>>5 != major {
			return nil, errors.New("invalid chunk in an indefinite CBOR string")
		}

		size, nested, err := d.argument(head & 0x1f)
		if err != nil || nested {
			return nil, errors.Join(err, errors.New("nested indefinite CBOR string"))
		}

		chunk, err := d.take(size)
		if err != nil {
			return nil, err
		}

		out = append(out, chunk...)
	}
}

func (d *decoder) simple(info byte) (any, error) {
	switch info {
	case 20:
		return false, nil
	case 21:
		return true, nil
	case 22, 23:
		return nil, nil //nolint:nilnil // CBOR null and undefined decode to nil.
	case 25:
		b, err := d.take(2)
		if err != nil {
			return nil, err
		}

		return half(binary.BigEndian.Uint16(b)), nil
	case 26:
		b, err := d.take(4)
		if err != nil {
			return nil, err
		}

		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 27:
		b, err := d.take(8)
		if err != nil {
			return nil, err
		}

		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	}

	return nil, fmt.Errorf("unsupported CBOR simple value %d", info)
}

// half decodes an IEEE 754 half-precision float.
func half(h uint16) float64 {
	exp, frac := int(h>>10&0x1f), float64(h&0x3ff)
	var v float64
	switch exp {
	case 0:
		v = frac * math.Pow(2, -24)
	case 31:
		v = math.Inf(1)
		if frac != 0 {
			v = math.NaN()
		}
	default:
		v = (1 + frac/1024) * math.Pow(2, float64(exp-15))
	}

	if h&0x8000 != 0 {
		v = -v
	}

	return v
}

// Encode renders a generated output type, or any JSON-like value, as CBOR.
func Encode(v any) []byte {
	var b bytes.Buffer
	encode(&b, reflect.ValueOf(v), false)
	return b.Bytes()
}

func head(b *bytes.Buffer, major byte, n uint64) {
	switch {
	case n < 24:
		b.WriteByte(major<<5 | byte(n))
	case n <= math.MaxUint8:
		b.Write([]byte{major<<5 | 24, byte(n)})
	case n <= math.MaxUint16:
		b.WriteByte(major<<5 | 25)
		_ = binary.Write(b, binary.BigEndian, uint16(n))
	case n <= math.MaxUint32:
		b.WriteByte(major<<5 | 26)
		_ = binary.Write(b, binary.BigEndian, uint32(n))
	default:
		b.WriteByte(major<<5 | 27)
		_ = binary.Write(b, binary.BigEndian, n)
	}
}

func encodeFloat(b *bytes.Buffer, f float64) {
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		if f >= 0 {
			head(b, 0, uint64(f))
		} else {
			head(b, 1, uint64(-f)-1)
		}

		return
	}

	b.WriteByte(7<<5 | 27)
	_ = binary.Write(b, binary.BigEndian, math.Float64bits(f))
}

func encodeString(b *bytes.Buffer, s string) {
	head(b, 3, uint64(len(s)))
	b.WriteString(s)
}

// encode writes v; timestamp marks a value tagged `cbor:"timestamp"`.
//
//nolint:gocyclo,cyclop // One case per kind.
func encode(b *bytes.Buffer, v reflect.Value, timestamp bool) {
	if !v.IsValid() {
		b.WriteByte(7<<5 | 22)
		return
	}

	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			b.WriteByte(7<<5 | 22)
			return
		}

		encode(b, v.Elem(), timestamp)
	case reflect.Struct:
		type field struct {
			value     reflect.Value
			name      string
			timestamp bool
		}

		var fields []field
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" || !f.IsExported() {
				continue
			}

			if name == "" {
				name = f.Name
			}

			fv := v.Field(i)
			if strings.Contains(opts, "omitempty") && fv.IsZero() {
				continue
			}

			fields = append(fields, field{fv, name, f.Tag.Get("cbor") == "timestamp"})
		}

		head(b, 5, uint64(len(fields)))
		for _, f := range fields {
			encodeString(b, f.name)
			encode(b, f.value, f.timestamp)
		}
	case reflect.Map:
		keys := make([]string, 0, v.Len())
		values := map[string]reflect.Value{}
		for _, k := range v.MapKeys() {
			keys = append(keys, k.String())
			values[k.String()] = v.MapIndex(k)
		}

		sort.Strings(keys)
		head(b, 5, uint64(len(keys)))
		for _, k := range keys {
			encodeString(b, k)
			encode(b, values[k], false)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			head(b, 2, uint64(v.Len())) // #nosec G115 -- lengths are not negative.
			b.Write(v.Bytes())
			return
		}

		head(b, 4, uint64(v.Len())) // #nosec G115 -- lengths are not negative.
		for i := range v.Len() {
			encode(b, v.Index(i), timestamp)
		}
	case reflect.String:
		encodeString(b, v.String())
	case reflect.Bool:
		if v.Bool() {
			b.WriteByte(7<<5 | 21)
		} else {
			b.WriteByte(7<<5 | 20)
		}
	case reflect.Int, reflect.Int32, reflect.Int64:
		encodeFloat(b, float64(v.Int()))
	case reflect.Float32, reflect.Float64:
		if timestamp {
			head(b, 6, 1)
		}

		encodeFloat(b, v.Float())
	default:
		b.WriteByte(7<<5 | 22)
	}
}

// Write encodes a successful response.
func Write(w http.ResponseWriter, status int, output any) {
	w.Header().Set("Content-Type", ContentType)
	w.Header().Set(Header, Protocol)
	w.WriteHeader(status)
	_, _ = w.Write(Encode(output))
}

// WriteError encodes e as {"__type": code, "message": message}; a QueryCode
// is also sent for query-compatible clients.
func WriteError(w http.ResponseWriter, e *awserror.Error) {
	if e.QueryCode != "" {
		w.Header().Set("X-Amzn-Query-Error", e.QueryCode+";"+e.Fault())
	}

	body := map[string]any{"__type": e.Code, "message": e.Message}
	maps.Copy(body, e.Fields)
	w.Header().Set("Content-Type", ContentType)
	w.Header().Set(Header, Protocol)
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(Encode(body))
}
