// Package wire implements the byte-level path between an MCP host and an
// upstream MCP server. Nothing in this package depends on capture, queue,
// or destination code: a panic or slow consumer downstream must never
// affect what bytes the host or upstream see.
package wire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// DefaultMaxFrameBytes is the default max size for a single capturable frame.
// Frames larger than this are still forwarded byte-for-byte by the tee
// (see Tee), but capture skips them.
const DefaultMaxFrameBytes = 16 * 1024 * 1024

// Errors returned by FrameScanner.
var (
	ErrFrameTooLarge  = errors.New("wire: frame exceeds capture max")
	ErrTruncatedFrame = errors.New("wire: truncated frame at EOF")
)

// FrameScanner reads newline-delimited JSON-RPC frames from an io.Reader.
//
// MCP-over-stdio frames are well-formed JSON objects terminated by a single
// `\n`. JSON itself never contains a bare newline outside of string escapes,
// so the newline is an unambiguous terminator. We use a customized
// bufio.Scanner with a raised token size cap.
type FrameScanner struct {
	sc *bufio.Scanner
}

// NewFrameScanner returns a scanner that returns one frame per call to Scan.
func NewFrameScanner(r io.Reader, maxFrameBytes int) *FrameScanner {
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	sc := bufio.NewScanner(r)
	// IMPORTANT: bufio.Scanner grows its buffer toward cap(initBuf) *before*
	// applying maxTokenSize, so the init-cap must not exceed the cap we
	// actually want to enforce. Otherwise oversize frames slip through.
	initCap := 64 * 1024
	if initCap > maxFrameBytes {
		initCap = maxFrameBytes
	}
	sc.Buffer(make([]byte, 0, initCap), maxFrameBytes)
	sc.Split(splitFrames)
	return &FrameScanner{sc: sc}
}

// splitFrames implements bufio.SplitFunc per the spec in §6.1.
func splitFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, ErrTruncatedFrame
	}
	return 0, nil, nil
}

// Scan reads the next frame. Returns false on EOF or error; check Err.
func (s *FrameScanner) Scan() bool { return s.sc.Scan() }

// Bytes returns the most recently scanned frame *without* the trailing newline.
// The slice is reused on subsequent Scan calls; copy if you need to retain.
func (s *FrameScanner) Bytes() []byte { return s.sc.Bytes() }

// Err returns the error, if any, that terminated Scan. bufio.ErrTooLong
// is translated to ErrFrameTooLarge.
func (s *FrameScanner) Err() error {
	err := s.sc.Err()
	if err == nil {
		return nil
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return ErrFrameTooLarge
	}
	return err
}
