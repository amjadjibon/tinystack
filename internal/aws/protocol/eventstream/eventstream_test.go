package eventstream

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func TestEncodeRoundTrip(t *testing.T) {
	values := []sdk.Value{
		sdk.BoolValue(true), sdk.BoolValue(false), sdk.Int8Value(-3), sdk.Int16Value(-1000),
		sdk.Int32Value(-100000), sdk.Int64Value(-10000000000),
		sdk.BytesValue{0, 255},
		sdk.StringValue("λ"),
		sdk.TimestampValue(time.UnixMilli(1700000000123).UTC()),
		sdk.UUIDValue{1, 2, 3},
	}
	message := Message{Payload: []byte("opaque payload")}
	for i, value := range values {
		message.Headers = append(message.Headers, sdk.Header{Name: string(rune('a' + i)), Value: value})
	}

	var buf bytes.Buffer
	if err := Encode(&buf, message); err != nil {
		t.Fatal(err)
	}

	got, err := sdk.NewDecoder().Decode(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, message) {
		t.Fatalf("round trip: %#v != %#v", got, message)
	}
}

func TestEncodeRejectsInvalidFrames(t *testing.T) {
	for _, message := range []Message{
		{Headers: sdk.Headers{{Name: "", Value: sdk.StringValue("value")}}},
		{Headers: sdk.Headers{{Name: "a", Value: sdk.StringValue(string(make([]byte, 65536)))}}},
		{Payload: make([]byte, 24*1024*1024+1)},
	} {
		var buf bytes.Buffer
		if err := Encode(&buf, message); err == nil || buf.Len() != 0 {
			t.Fatal("invalid frame was written")
		}
	}
}

func TestEncodeWriteError(t *testing.T) {
	if err := Encode(failingWriter{}, Event("event", "test", nil)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestChecksums(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, Event("event", "test", []byte("payload"))); err != nil {
		t.Fatal(err)
	}

	for _, offset := range []int{0, 8, 12, buf.Len() - 1} {
		frame := bytes.Clone(buf.Bytes())
		frame[offset] ^= 1
		if _, err := sdk.NewDecoder().Decode(bytes.NewReader(frame), nil); err == nil {
			t.Fatalf("corrupt frame at %d accepted", offset)
		}
	}
}
