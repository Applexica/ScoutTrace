package wire

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
)

// Direction tags whether a frame travelled host→upstream or upstream→host.
type Direction int

const (
	DirHostToUpstream Direction = iota
	DirUpstreamToHost
)

// Frame is a captured wire frame with direction metadata. The bytes are
// always a fresh copy independent of any read buffer.
type Frame struct {
	Dir   Direction
	Bytes []byte
}

// TeeStats are atomically-updated counters exposed via Counters().
type TeeStats struct {
	Forwarded      uint64
	CapturedSent   uint64
	CaptureDrops   uint64
	OversizeFrames uint64
	WriteErrors    uint64
	ParseErrors    uint64
}

// Tee copies one direction of stdio.
//
// Wire-path invariant (§2.2 of TECHNICAL_DESIGN): every byte read from src
// is written to dst BEFORE the capture pipeline sees it. We do not use
// bufio.Scanner because Scanner buffers internally and can swallow the
// prefix of an oversized frame on `ErrTooLong`. Instead we read raw bytes
// and forward them immediately, splitting on '\n' only for capture
// accounting. Frames larger than MaxFrame are still forwarded byte-for-byte
// — capture just stops accumulating until the next newline.
type Tee struct {
	Src      io.Reader
	Dst      io.Writer
	Dir      Direction
	CapCh    chan<- Frame
	MaxFrame int
	Stats    *TeeStats
	OnPanic  func(any) // optional panic hook on capture publish
}

// Run copies until EOF or a write error.
func (t *Tee) Run() error {
	if t.MaxFrame <= 0 {
		t.MaxFrame = DefaultMaxFrameBytes
	}
	const readBuf = 64 * 1024
	buf := make([]byte, readBuf)
	frame := make([]byte, 0, 4096)
	skipping := false
	for {
		n, err := t.Src.Read(buf)
		if n > 0 {
			// FORWARD FIRST — the wire path must never wait on capture.
			if _, werr := t.Dst.Write(buf[:n]); werr != nil {
				atomic.AddUint64(&t.Stats.WriteErrors, 1)
				return werr
			}

			// Walk the just-read bytes for capture accounting.
			data := buf[:n]
			for len(data) > 0 {
				idx := bytes.IndexByte(data, '\n')
				if idx < 0 {
					// Tail (no newline yet): accumulate or drop.
					if !skipping {
						if len(frame)+len(data) > t.MaxFrame {
							atomic.AddUint64(&t.Stats.OversizeFrames, 1)
							skipping = true
							frame = frame[:0]
						} else {
							frame = append(frame, data...)
						}
					}
					break
				}
				// We have a complete frame ending at idx.
				if !skipping {
					if len(frame)+idx > t.MaxFrame {
						atomic.AddUint64(&t.Stats.OversizeFrames, 1)
					} else {
						frame = append(frame, data[:idx]...)
						atomic.AddUint64(&t.Stats.Forwarded, 1)
						t.publish(frame)
					}
				}
				// Reset for next frame.
				frame = frame[:0]
				skipping = false
				data = data[idx+1:]
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// publish copies the frame and best-effort-sends to capture. Drops if the
// channel is full; recovers from panics.
func (t *Tee) publish(frame []byte) {
	defer func() {
		if r := recover(); r != nil {
			atomic.AddUint64(&t.Stats.CaptureDrops, 1)
			if t.OnPanic != nil {
				t.OnPanic(r)
			}
		}
	}()
	if t.CapCh == nil {
		return
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	select {
	case t.CapCh <- Frame{Dir: t.Dir, Bytes: cp}:
		atomic.AddUint64(&t.Stats.CapturedSent, 1)
	default:
		atomic.AddUint64(&t.Stats.CaptureDrops, 1)
	}
}
