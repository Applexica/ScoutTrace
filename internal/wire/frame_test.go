package wire

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestFrameRoundtrip(t *testing.T) {
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"a"}`,
		`{"jsonrpc":"2.0","id":2,"method":"b","params":{"x":1}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"ok":true}}`,
	}
	in := strings.NewReader(strings.Join(frames, "\n") + "\n")
	sc := NewFrameScanner(in, 0)
	var got []string
	for sc.Scan() {
		got = append(got, string(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != len(frames) {
		t.Fatalf("got %d frames, want %d", len(got), len(frames))
	}
	for i, f := range frames {
		if got[i] != f {
			t.Errorf("frame %d differs:\n got: %s\nwant: %s", i, got[i], f)
		}
	}
}

func TestFrameTruncatedAtEOF(t *testing.T) {
	// "abc" with no trailing newline → ErrTruncatedFrame after EOF.
	sc := NewFrameScanner(strings.NewReader("abc"), 0)
	if sc.Scan() {
		t.Fatalf("did not expect a successful Scan: got %q", sc.Bytes())
	}
	if !errors.Is(sc.Err(), ErrTruncatedFrame) {
		t.Fatalf("Err = %v, want ErrTruncatedFrame", sc.Err())
	}
}

func TestFrameOversize(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 1024)
	sc := NewFrameScanner(bytes.NewReader(append(big, '\n')), 256)
	for sc.Scan() {
		t.Fatalf("expected oversize error, got frame: %d bytes", len(sc.Bytes()))
	}
	if !errors.Is(sc.Err(), ErrFrameTooLarge) {
		t.Fatalf("Err = %v, want ErrFrameTooLarge", sc.Err())
	}
}

func TestTeeForwardsBeforeCapture(t *testing.T) {
	src := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"a"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"b"}` + "\n")
	var out bytes.Buffer
	stats := &TeeStats{}
	capCh := make(chan Frame, 16)
	tee := &Tee{Src: src, Dst: &out, Dir: DirHostToUpstream, CapCh: capCh, Stats: stats}
	if err := tee.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	expected := `{"jsonrpc":"2.0","id":1,"method":"a"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"b"}` + "\n"
	if out.String() != expected {
		t.Fatalf("forwarded bytes differ:\n got: %q\nwant: %q", out.String(), expected)
	}
	close(capCh)
	var captured []string
	for f := range capCh {
		captured = append(captured, string(f.Bytes))
	}
	if len(captured) != 2 {
		t.Errorf("captured %d frames, want 2", len(captured))
	}
	if stats.Forwarded != 2 || stats.CapturedSent != 2 {
		t.Errorf("counters: forwarded=%d sent=%d", stats.Forwarded, stats.CapturedSent)
	}
}

// blockingWriter never returns from Write until released. Used to verify
// that capture channel saturation does NOT make the wire path block.
type slowReader struct {
	r []byte
	i int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.r) {
		return 0, errEOF
	}
	n := copy(p, s.r[s.i:])
	s.i += n
	return n, nil
}

var errEOF = errors.New("EOF")

func TestTeeNonBlockingOnFullCapture(t *testing.T) {
	// Capture channel has capacity 0; every send drops.
	frames := strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"x"}`+"\n", 5)
	src := strings.NewReader(frames)
	var out bytes.Buffer
	stats := &TeeStats{}
	capCh := make(chan Frame) // unbuffered, never read
	tee := &Tee{Src: src, Dst: &out, Dir: DirHostToUpstream, CapCh: capCh, Stats: stats}
	if err := tee.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != len(frames) {
		t.Fatalf("forwarded %d bytes, want %d", out.Len(), len(frames))
	}
	if stats.CaptureDrops != 5 {
		t.Errorf("CaptureDrops = %d, want 5", stats.CaptureDrops)
	}
}

// TestTeeForwardsOversizedFramesByteForByte verifies wire-transparency:
// even when a frame exceeds MaxFrame and capture skips it, every byte
// must still reach dst (PRD AC-W2).
func TestTeeForwardsOversizedFramesByteForByte(t *testing.T) {
	// One small frame, one oversized frame, one small frame.
	small1 := `{"jsonrpc":"2.0","id":1,"method":"a"}`
	small2 := `{"jsonrpc":"2.0","id":2,"method":"b"}`
	huge := bytes.Repeat([]byte("X"), 4096)
	input := small1 + "\n" + string(huge) + "\n" + small2 + "\n"

	src := strings.NewReader(input)
	var out bytes.Buffer
	stats := &TeeStats{}
	capCh := make(chan Frame, 16)
	tee := &Tee{Src: src, Dst: &out, Dir: DirHostToUpstream, CapCh: capCh, Stats: stats, MaxFrame: 256}
	if err := tee.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != input {
		t.Fatalf("forwarded bytes differ; got %d want %d", out.Len(), len(input))
	}
	if stats.OversizeFrames != 1 {
		t.Errorf("OversizeFrames = %d, want 1", stats.OversizeFrames)
	}
	close(capCh)
	captured := []string{}
	for f := range capCh {
		captured = append(captured, string(f.Bytes))
	}
	if len(captured) != 2 {
		t.Errorf("captured %d frames, want 2 (oversize skipped); got %v", len(captured), captured)
	}
	if len(captured) >= 1 && captured[0] != small1 {
		t.Errorf("captured[0] = %q, want %q", captured[0], small1)
	}
	if len(captured) >= 2 && captured[1] != small2 {
		t.Errorf("captured[1] = %q, want %q", captured[1], small2)
	}
}
