package protocol

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestJSONLDecoderReadsFramesAndSkipsBlankLines(t *testing.T) {
	decoder := NewJSONLDecoder(strings.NewReader("\n  \n{\"type\":\"a\"}\n{\"type\":\"b\",\"n\":2}\n"), 1024)

	var first map[string]any
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first["type"] != "a" {
		t.Fatalf("first frame = %#v", first)
	}

	var second map[string]any
	if err := decoder.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second["type"] != "b" || second["n"] != float64(2) {
		t.Fatalf("second frame = %#v", second)
	}
	if err := decoder.Decode(&second); !errors.Is(err, io.EOF) {
		t.Fatalf("final Decode error = %v, want EOF", err)
	}
}

func TestJSONLDecoderReportsMalformedJSON(t *testing.T) {
	decoder := NewJSONLDecoder(strings.NewReader("{not-json}\n"), 1024)
	var frame map[string]any
	err := decoder.Decode(&frame)
	var frameErr *FrameError
	if !errors.As(err, &frameErr) {
		t.Fatalf("error = %T %v, want *FrameError", err, err)
	}
	if frameErr.Kind != ErrMalformedJSON {
		t.Fatalf("kind = %q, want %q", frameErr.Kind, ErrMalformedJSON)
	}
}

func TestJSONLDecoderRejectsOversizedFrame(t *testing.T) {
	decoder := NewJSONLDecoder(strings.NewReader("{\"value\":\""+strings.Repeat("x", 256)+"\"}\n"), 64)
	var frame map[string]any
	err := decoder.Decode(&frame)
	var frameErr *FrameError
	if !errors.As(err, &frameErr) {
		t.Fatalf("error = %T %v, want *FrameError", err, err)
	}
	if frameErr.Kind != ErrFrameTooLarge {
		t.Fatalf("kind = %q, want %q", frameErr.Kind, ErrFrameTooLarge)
	}
}

func TestJSONLDecoderAcceptsFinalFrameWithoutNewline(t *testing.T) {
	decoder := NewJSONLDecoder(strings.NewReader("{\"ok\":true}"), 64)
	var frame struct {
		OK bool `json:"ok"`
	}
	if err := decoder.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if !frame.OK {
		t.Fatal("final frame was not decoded")
	}
}
