package proto

import (
	"encoding/binary"
	"errors"
)

// BodyWriter appends XBridge body fields in little-endian order, matching the
// C++ XBridgePacket::append helpers.
type BodyWriter struct {
	buf []byte
}

func NewBodyWriter() *BodyWriter { return &BodyWriter{} }

func (w *BodyWriter) Uint32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *BodyWriter) Uint64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// Uint16 appends a little-endian uint16 (matches C++ append(uint16_t)).
func (w *BodyWriter) Uint16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// PubKey appends a 33-byte compressed secp256k1 public key.
func (w *BodyWriter) PubKey(p [33]byte) { w.buf = append(w.buf, p[:]...) }

// Bytes appends raw bytes unchanged.
func (w *BodyWriter) Bytes(b []byte) { w.buf = append(w.buf, b...) }

// String appends a null-terminated string (C++ append(std::string) writes bytes + 0).
func (w *BodyWriter) String(s string) {
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

// Hash appends a raw 32-byte value (e.g. a uint256 in Bitcoin internal LE order).
func (w *BodyWriter) Hash(h [32]byte) { w.buf = append(w.buf, h[:]...) }

// Addr appends a raw 20-byte address (uint160).
func (w *BodyWriter) Addr(a [20]byte) { w.buf = append(w.buf, a[:]...) }

// Currency appends an 8-byte currency code (ASCII, left-aligned, null-padded).
func (w *BodyWriter) Currency(s string) {
	var b [8]byte
	copy(b[:], []byte(s))
	w.buf = append(w.buf, b[:]...)
}

func (w *BodyWriter) Payload() []byte { return w.buf }

// BodyReader reads XBridge body fields sequentially.
type BodyReader struct {
	data []byte
	pos  int
}

func NewBodyReader(b []byte) *BodyReader { return &BodyReader{data: b} }

func (r *BodyReader) remaining() int { return len(r.data) - r.pos }

func (r *BodyReader) Uint32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, errors.New("xbridge: body underflow (uint32)")
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *BodyReader) Uint64() (uint64, error) {
	if r.remaining() < 8 {
		return 0, errors.New("xbridge: body underflow (uint64)")
	}
	v := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *BodyReader) Uint16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, errors.New("xbridge: body underflow (uint16)")
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *BodyReader) PubKey() ([33]byte, error) {
	var p [33]byte
	b, err := r.Bytes(33)
	if err != nil {
		return p, err
	}
	copy(p[:], b)
	return p, nil
}

func (r *BodyReader) Bytes(n int) ([]byte, error) {
	if r.remaining() < n {
		return nil, errors.New("xbridge: body underflow (bytes)")
	}
	b := make([]byte, n)
	copy(b, r.data[r.pos:r.pos+n])
	r.pos += n
	return b, nil
}

func (r *BodyReader) String() (string, error) {
	for i := r.pos; i < len(r.data); i++ {
		if r.data[i] == 0 {
			s := string(r.data[r.pos:i])
			r.pos = i + 1
			return s, nil
		}
	}
	return "", errors.New("xbridge: unterminated string in body")
}

func (r *BodyReader) Hash() ([32]byte, error) {
	var h [32]byte
	b, err := r.Bytes(32)
	if err != nil {
		return h, err
	}
	copy(h[:], b)
	return h, nil
}

func (r *BodyReader) Addr() ([20]byte, error) {
	var a [20]byte
	b, err := r.Bytes(20)
	if err != nil {
		return a, err
	}
	copy(a[:], b)
	return a, nil
}

func (r *BodyReader) Currency() (string, error) {
	b, err := r.Bytes(8)
	if err != nil {
		return "", err
	}
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	return string(b[:end]), nil
}
