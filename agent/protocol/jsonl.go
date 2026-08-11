// Package protocol provides Agent-agnostic protocol framing primitives.
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const defaultMaxFrameSize = 10 * 1024 * 1024

// ErrorKind identifies a framing failure.
type ErrorKind string

const (
	// ErrFrameTooLarge means one line exceeded the configured frame bound.
	ErrFrameTooLarge ErrorKind = "frame_too_large"
	// ErrMalformedJSON means a complete frame was not valid JSON.
	ErrMalformedJSON ErrorKind = "malformed_json"
)

// FrameError is a typed JSONL framing error.
type FrameError struct {
	Kind ErrorKind
	Err  error
}

func (e *FrameError) Error() string {
	if e == nil {
		return "jsonl frame error"
	}
	return fmt.Sprintf("jsonl %s: %v", e.Kind, e.Err)
}

// Unwrap exposes the underlying scanner or JSON error.
func (e *FrameError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// JSONLDecoder decodes one bounded non-empty JSON line per Decode call.
type JSONLDecoder struct {
	scanner *bufio.Scanner
}

// NewJSONLDecoder constructs a decoder. A non-positive maxSize selects the
// default 10 MiB bound.
func NewJSONLDecoder(reader io.Reader, maxSize int) *JSONLDecoder {
	if maxSize <= 0 {
		maxSize = defaultMaxFrameSize
	}
	initialSize := maxSize
	if initialSize > 64*1024 {
		initialSize = 64 * 1024
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, initialSize), maxSize)
	return &JSONLDecoder{scanner: scanner}
}

// Decode skips blank lines and unmarshals the next frame into dst.
func (d *JSONLDecoder) Decode(dst any) error {
	if d == nil || d.scanner == nil {
		return io.EOF
	}
	for d.scanner.Scan() {
		frame := strings.TrimSpace(d.scanner.Text())
		if frame == "" {
			continue
		}
		if err := json.Unmarshal([]byte(frame), dst); err != nil {
			return &FrameError{Kind: ErrMalformedJSON, Err: err}
		}
		return nil
	}
	if err := d.scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return &FrameError{Kind: ErrFrameTooLarge, Err: err}
		}
		return fmt.Errorf("jsonl scan: %w", err)
	}
	return io.EOF
}
