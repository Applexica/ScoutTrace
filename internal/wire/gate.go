package wire

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
)

// Gate inspects host→upstream frames before they are forwarded.
//
// Implementations must be fast and non-blocking. A typical Gate consults
// an in-memory halt cache and either returns Allow{} (forward normally)
// or Block{Reply: <JSON-RPC error bytes>} (synthesize a response back
// to the host instead of forwarding).
//
// Inspect is called once per complete frame. The frame bytes do NOT
// include the trailing newline. The Gate must not retain the slice
// past return — the caller may reuse it.
type Gate interface {
	Inspect(frame []byte) Decision
}

// Decision controls what happens to a single frame.
type Decision struct {
	// Forward, when true, forwards the original frame to upstream
	// unchanged. False refuses forwarding.
	Forward bool
	// Reply is the synthetic response bytes to write back to the host
	// (no trailing newline; the runtime adds one). Only honored when
	// Forward is false.
	Reply []byte
	// Capture, when true, still publishes the original frame to the
	// capture channel even when Forward is false. Useful for recording
	// blocked attempts.
	Capture bool
}

// Allow returns a Decision that forwards the frame unchanged.
func Allow() Decision { return Decision{Forward: true, Capture: true} }

// Block returns a Decision that refuses forwarding and writes reply
// back to the host. Capture is enabled so blocked attempts show up in
// telemetry.
func Block(reply []byte) Decision { return Decision{Forward: false, Reply: reply, Capture: true} }

// GatedForwarder is an alternative to Tee for the host→upstream
// direction when halt enforcement is active. It parses each frame from
// the source, consults the Gate, and either forwards normally or
// writes a synthetic reply back to the host.
//
// The forward path is frame-based rather than byte-based: this is the
// trade-off for being able to refuse forwarding. We still honor the
// "forward before publish to capture" invariant for allowed frames.
type GatedForwarder struct {
	Src        io.Reader  // host stdin
	Upstream   io.Writer  // upstream stdin
	HostBack   io.Writer  // upstream stdout sink (synthetic replies)
	Gate       Gate
	CapCh      chan<- Frame
	MaxFrame   int
	Stats      *TeeStats
	OnPanic    func(any)
	// HostBackLock, when non-nil, is held during writes to HostBack so
	// the forwarder doesn't interleave with the legitimate upstream→host
	// Tee. Callers must construct a sync.Mutex shared between both.
	HostBackLock interface{ Lock(); Unlock() }
}

// Run reads frames from Src and forwards (or refuses) each. Returns
// nil on clean EOF, otherwise the underlying read/write error.
func (g *GatedForwarder) Run() error {
	if g.MaxFrame <= 0 {
		g.MaxFrame = DefaultMaxFrameBytes
	}
	if g.Stats == nil {
		g.Stats = &TeeStats{}
	}
	sc := NewFrameScanner(g.Src, g.MaxFrame)
	for sc.Scan() {
		frame := sc.Bytes()
		dec := Allow()
		if g.Gate != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						if g.OnPanic != nil {
							g.OnPanic(r)
						}
					}
				}()
				dec = g.Gate.Inspect(frame)
			}()
		}
		if dec.Forward {
			// Forward unchanged with the trailing newline restored.
			if err := g.writeFrame(g.Upstream, frame); err != nil {
				atomic.AddUint64(&g.Stats.WriteErrors, 1)
				return err
			}
			atomic.AddUint64(&g.Stats.Forwarded, 1)
		} else {
			// Refuse forwarding; write synthetic reply to the host
			// stdout sink. The reply is best-effort: if HostBack is
			// nil we silently drop (still publish to capture).
			if len(dec.Reply) > 0 && g.HostBack != nil {
				if g.HostBackLock != nil {
					g.HostBackLock.Lock()
				}
				err := g.writeFrame(g.HostBack, dec.Reply)
				if g.HostBackLock != nil {
					g.HostBackLock.Unlock()
				}
				if err != nil {
					atomic.AddUint64(&g.Stats.WriteErrors, 1)
					return err
				}
			}
		}
		if dec.Capture {
			g.publish(frame)
		}
	}
	err := sc.Err()
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (g *GatedForwarder) writeFrame(w io.Writer, frame []byte) error {
	var buf bytes.Buffer
	buf.Grow(len(frame) + 1)
	buf.Write(frame)
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func (g *GatedForwarder) publish(frame []byte) {
	defer func() {
		if r := recover(); r != nil {
			atomic.AddUint64(&g.Stats.CaptureDrops, 1)
			if g.OnPanic != nil {
				g.OnPanic(r)
			}
		}
	}()
	if g.CapCh == nil {
		return
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	select {
	case g.CapCh <- Frame{Dir: DirHostToUpstream, Bytes: cp}:
		atomic.AddUint64(&g.Stats.CapturedSent, 1)
	default:
		atomic.AddUint64(&g.Stats.CaptureDrops, 1)
	}
}
