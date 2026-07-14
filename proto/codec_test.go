package proto

import "testing"

func TestPacketRoundTrip(t *testing.T) {
	body := NewBodyWriter()
	body.Uint32(1)
	body.Uint64(123456)
	body.String("hello")
	body.Currency("BTC")
	body.Hash([32]byte{0xaa})
	body.Addr([20]byte{0xbb})

	p := NewPacket(XbcTransaction, body.Payload())
	if p.Size != uint32(len(body.Payload())) {
		t.Fatalf("size mismatch: %d != %d", p.Size, len(body.Payload()))
	}

	wire := p.Marshal()
	if len(wire) != HeaderSize+len(body.Payload()) {
		t.Fatalf("wire len = %d, want %d", len(wire), HeaderSize+len(body.Payload()))
	}

	got, err := Unmarshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != XbcTransaction {
		t.Fatalf("command = %v", got.Command)
	}
	if got.Size != p.Size {
		t.Fatalf("size = %d, want %d", got.Size, p.Size)
	}

	r := NewBodyReader(got.Body)
	if v, _ := r.Uint32(); v != 1 {
		t.Fatalf("uint32 = %d", v)
	}
	if v, _ := r.Uint64(); v != 123456 {
		t.Fatalf("uint64 = %d", v)
	}
	if s, _ := r.String(); s != "hello" {
		t.Fatalf("string = %q", s)
	}
	if c, _ := r.Currency(); c != "BTC" {
		t.Fatalf("currency = %q", c)
	}
	if h, _ := r.Hash(); h[0] != 0xaa {
		t.Fatalf("hash[0] = %x", h[0])
	}
	if a, _ := r.Addr(); a[0] != 0xbb {
		t.Fatalf("addr[0] = %x", a[0])
	}

	// Digest must be deterministic.
	if p.Digest() != p.Digest() {
		t.Fatal("digest not deterministic")
	}

	// Header layout offsets must match the C++ source.
	if PubkeyOffset != 20 || SigOffset != 53 || HeaderSize != 129 {
		t.Fatalf("header layout wrong: pubkey@%d sig@%d size=%d", PubkeyOffset, SigOffset, HeaderSize)
	}
}
