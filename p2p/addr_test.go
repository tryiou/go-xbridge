package p2p

import (
	"net"
	"testing"
)

// TestAddrRoundTrip verifies MarshalAddr/ParseAddr round-trip for empty,
// single, and multi-entry payloads, including IPv4 and IPv6 addresses.
func TestAddrRoundTrip(t *testing.T) {
	cases := [][]AddrEntry{
		{}, // empty
		{
			{Time: 1, Services: 0, IP: net.ParseIP("1.2.3.4"), Port: 41412},
		},
		{
			{Time: 1234, Services: 1, IP: net.ParseIP("1.2.3.4"), Port: 41412},
			{Time: 5678, Services: 9, IP: net.ParseIP("2001:db8::1"), Port: 41474},
		},
	}
	for i, want := range cases {
		b := MarshalAddr(want)
		got, err := ParseAddr(b)
		if err != nil {
			t.Fatalf("case %d: ParseAddr: %v", i, err)
		}
		if len(got) != len(want) {
			t.Fatalf("case %d: len = %d, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j].Time != want[j].Time {
				t.Errorf("case %d entry %d: time = %d, want %d", i, j, got[j].Time, want[j].Time)
			}
			if got[j].Services != want[j].Services {
				t.Errorf("case %d entry %d: services = %d, want %d", i, j, got[j].Services, want[j].Services)
			}
			if got[j].Port != want[j].Port {
				t.Errorf("case %d entry %d: port = %d, want %d", i, j, got[j].Port, want[j].Port)
			}
			// IPv4 parses back to a 4-byte form; IPv6 stays 16-byte. Compare
			// with Equal so v4->v6-mapped->v4 normalisation matches.
			if !got[j].IP.Equal(want[j].IP) {
				t.Errorf("case %d entry %d: ip = %v, want %v", i, j, got[j].IP, want[j].IP)
			}
		}
	}
}

// TestAddrEmptyPayload ensures an explicitly-empty addr (single 0x00 count byte)
// decodes to zero entries without error.
func TestAddrEmptyPayload(t *testing.T) {
	got, err := ParseAddr([]byte{0x00})
	if err != nil {
		t.Fatalf("ParseAddr(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty addr len = %d, want 0", len(got))
	}
}

// TestAddrRecordCap asserts a declared count above MaxAddrRecords is rejected,
// matching C++ Misbehaving on an oversized addr message
// (net_processing.cpp:1825-1830, MAX_ADDR_TO_SEND net.h:53).
func TestAddrRecordCap(t *testing.T) {
	// Declared 1001 records (0xfd 0xe9 0x03); rejected before any record decode.
	oversized := []byte{0xfd, 0xe9, 0x03}
	if _, err := ParseAddr(oversized); err == nil {
		t.Fatal("expected error for addr payload declaring >1000 records")
	}
	// Exactly the cap is still accepted (zero records follow the count).
	if _, err := ParseAddr(writeVarInt(MaxAddrRecords)); err != nil {
		t.Fatalf("cap payload: %v", err)
	}
}
