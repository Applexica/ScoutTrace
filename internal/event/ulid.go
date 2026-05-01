package event

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// ULID: 48-bit ms timestamp + 80 bits of randomness, encoded in 26 chars
// of Crockford base32. Lexicographic order matches generation time.
//
// We provide a monotonic generator: when two IDs are minted in the same
// millisecond, the random component is incremented rather than re-randomized,
// preserving sort order within a session.
const ulidEncoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	ulidMu       sync.Mutex
	lastULIDms   uint64
	lastULIDrand [10]byte
)

// NewULID returns a new lexicographically-monotonic ULID string.
func NewULID() string {
	return NewULIDAt(time.Now())
}

// NewULIDAt returns a ULID anchored to the supplied wall-clock time.
func NewULIDAt(t time.Time) string {
	ulidMu.Lock()
	defer ulidMu.Unlock()

	ms := uint64(t.UnixMilli())
	var rnd [10]byte
	if ms == lastULIDms {
		// Same ms: increment the previous random component as a 80-bit
		// big-endian integer so the new ID sorts after the previous one.
		copy(rnd[:], lastULIDrand[:])
		for i := 9; i >= 0; i-- {
			rnd[i]++
			if rnd[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(rnd[:]); err != nil {
			// rand.Read on modern OSes does not fail; treat as panic.
			panic("scouttrace/event: crypto/rand failed: " + err.Error())
		}
		lastULIDms = ms
	}
	copy(lastULIDrand[:], rnd[:])
	return encodeULID(ms, rnd)
}

func encodeULID(ms uint64, rnd [10]byte) string {
	var buf [26]byte
	// Timestamp: 10 chars (50 bits, but we only use 48; high bits zero).
	buf[0] = ulidEncoding[(ms>>45)&0x1f]
	buf[1] = ulidEncoding[(ms>>40)&0x1f]
	buf[2] = ulidEncoding[(ms>>35)&0x1f]
	buf[3] = ulidEncoding[(ms>>30)&0x1f]
	buf[4] = ulidEncoding[(ms>>25)&0x1f]
	buf[5] = ulidEncoding[(ms>>20)&0x1f]
	buf[6] = ulidEncoding[(ms>>15)&0x1f]
	buf[7] = ulidEncoding[(ms>>10)&0x1f]
	buf[8] = ulidEncoding[(ms>>5)&0x1f]
	buf[9] = ulidEncoding[ms&0x1f]
	// Randomness: 16 chars from 10 bytes.
	r := binary.BigEndian.Uint64(rnd[0:8])
	buf[10] = ulidEncoding[(r>>59)&0x1f]
	buf[11] = ulidEncoding[(r>>54)&0x1f]
	buf[12] = ulidEncoding[(r>>49)&0x1f]
	buf[13] = ulidEncoding[(r>>44)&0x1f]
	buf[14] = ulidEncoding[(r>>39)&0x1f]
	buf[15] = ulidEncoding[(r>>34)&0x1f]
	buf[16] = ulidEncoding[(r>>29)&0x1f]
	buf[17] = ulidEncoding[(r>>24)&0x1f]
	buf[18] = ulidEncoding[(r>>19)&0x1f]
	buf[19] = ulidEncoding[(r>>14)&0x1f]
	buf[20] = ulidEncoding[(r>>9)&0x1f]
	buf[21] = ulidEncoding[(r>>4)&0x1f]
	hi5 := (r & 0x0f) << 1
	tail := binary.BigEndian.Uint16(rnd[8:10])
	buf[22] = ulidEncoding[hi5|uint64(tail>>15)&0x01]
	buf[23] = ulidEncoding[(tail>>10)&0x1f]
	buf[24] = ulidEncoding[(tail>>5)&0x1f]
	buf[25] = ulidEncoding[tail&0x1f]
	return string(buf[:])
}

// ValidateULID returns nil if s is a syntactically valid ULID.
func ValidateULID(s string) error {
	if len(s) != 26 {
		return errors.New("ulid: must be 26 chars")
	}
	for i := 0; i < 26; i++ {
		c := s[i]
		ok := false
		for j := 0; j < len(ulidEncoding); j++ {
			if c == ulidEncoding[j] {
				ok = true
				break
			}
		}
		if !ok {
			return errors.New("ulid: invalid character")
		}
	}
	return nil
}
