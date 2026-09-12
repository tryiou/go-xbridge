package proto

// Alignment regression tests: fixed-size bodies must reject trailing bytes
// and pay-txid bodies must enforce the C++ 52/56 < size <= 1000 bounds
// (xbridgesession.cpp:1328,1554,1680,1786,1898,2853,3026,3110,3209,3293,3437,3778).

import (
	"strings"
	"testing"
)

func mustMarshal(t *testing.T, body interface {
	Marshal() []byte
}) []byte {
	t.Helper()
	return body.Marshal()
}

func TestFixedBodiesRejectTrailingBytes(t *testing.T) {
	hold := &HoldBody{FromAmount: 1, ToAmount: 2}
	if err := (&HoldBody{}).Unmarshal(append(mustMarshal(t, hold), 0x00)); err == nil {
		t.Error("HoldBody with trailing byte must be rejected (C++ exact 68)")
	}
	holdApply := &HoldApplyBody{}
	if err := holdApply.Unmarshal(append(mustMarshal(t, holdApply), 0x00)); err == nil {
		t.Error("HoldApplyBody with trailing byte must be rejected (C++ exact 72)")
	}
	init := &InitBody{FromCurrency: "BTC", ToCurrency: "LTC"}
	if err := (&InitBody{}).Unmarshal(append(mustMarshal(t, init), 0x00)); err == nil {
		t.Error("InitBody with trailing byte must be rejected (C++ exact 144)")
	}
	initialized := &InitializedBody{}
	if err := initialized.Unmarshal(append(mustMarshal(t, initialized), 0x00)); err == nil {
		t.Error("InitializedBody with trailing byte must be rejected (C++ exact 72)")
	}
	createA := &CreateABody{}
	if err := createA.Unmarshal(append(mustMarshal(t, createA), 0x00)); err == nil {
		t.Error("CreateABody with trailing byte must be rejected (C++ exact 85)")
	}
	fin := &FinishedBody{}
	if err := fin.Unmarshal(append(mustMarshal(t, fin), 0x00)); err == nil {
		t.Error("FinishedBody with trailing byte must be rejected (C++ exact 32)")
	}
	// Exact-size bodies still decode.
	if err := (&HoldBody{}).Unmarshal(mustMarshal(t, hold)); err != nil {
		t.Errorf("exact HoldBody must decode: %v", err)
	}
}

func TestPayTxBodiesEnforceLengthBounds(t *testing.T) {
	// Oversized pay-txid (>1000 total) must be rejected.
	big := &ConfirmBBody{APayTxID: strings.Repeat("a", 2000)}
	if err := (&ConfirmBBody{}).Unmarshal(mustMarshal(t, big)); err == nil {
		t.Error("ConfirmBBody over 1000 bytes must be rejected (C++ :3110)")
	}
	bigA := &ConfirmedABody{APayTxID: strings.Repeat("a", 2000)}
	if err := (&ConfirmedABody{}).Unmarshal(mustMarshal(t, bigA)); err == nil {
		t.Error("ConfirmedABody over 1000 bytes must be rejected (C++ :3026)")
	}
	bigB := &ConfirmedBBody{BPayTxID: strings.Repeat("b", 2000)}
	if err := (&ConfirmedBBody{}).Unmarshal(mustMarshal(t, bigB)); err == nil {
		t.Error("ConfirmedBBody over 1000 bytes must be rejected (C++ :3209)")
	}
	// Normal pay-txids still decode.
	okB := &ConfirmBBody{APayTxID: strings.Repeat("a", 64)}
	if err := (&ConfirmBBody{}).Unmarshal(mustMarshal(t, okB)); err != nil {
		t.Errorf("normal ConfirmBBody must decode: %v", err)
	}
	okA := &ConfirmABody{BDepositTxID: strings.Repeat("c", 64), BLockTime: 100}
	if err := (&ConfirmABody{}).Unmarshal(mustMarshal(t, okA)); err != nil {
		t.Errorf("normal ConfirmABody must decode: %v", err)
	}
}
