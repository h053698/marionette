package host

import (
	"encoding/binary"
	"fmt"
	"io"
)

const MaxPacket = 16 << 20
const MaxControl = 1 << 20

func ReadPacket(r io.Reader) ([]byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxPacket {
		return nil, fmt.Errorf("video payload exceeds %d bytes", MaxPacket)
	}
	if h[0] < 1 || h[0] > 4 {
		return nil, fmt.Errorf("unknown video packet type %d", h[0])
	}
	if h[0] == 3 && n < 10 {
		return nil, fmt.Errorf("truncated frame header")
	}
	b := make([]byte, 5+int(n))
	copy(b, h[:])
	_, err := io.ReadFull(r, b[5:])
	return b, err
}
func keyframe(b []byte) bool { return len(b) >= 14 && b[0] == 3 && b[13]&1 != 0 }
