package api

import (
	"encoding/hex"
	"testing"

	"go-xbridge/coins"
)

// Tests for the own-deposit mempool watch task's scan bookkeeping: the
// incremental seen-set must only advance on txids whose raw bytes were
// actually fetched — an RPC failure (mempool churn) must leave the txid
// unrecorded so a later sweep can re-examine it.

// TestOwnWatchScanMarksOnlyFetchedTxids proves a GetRawTransaction failure
// does not mark the txid as scanned: the seen-set merge in the apply would
// otherwise hide that txid from every future sweep for the session's whole
// lifetime (even if it re-enters the mempool), degrading recovery to the
// locktime refund.
func TestOwnWatchScanMarksOnlyFetchedTxids(t *testing.T) {
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	conn.mempoolTxids = []string{"failedtx", "fetchedtx"}
	tx := &coins.Tx{Version: 1}
	conn.setRawTx("fetchedtx", hex.EncodeToString(tx.Serialize()))

	res, err := runOwnWatchTask(conn, map[string]struct{}{}, [20]byte{}, "deptxid", 0, false)
	if err != nil {
		t.Fatalf("runOwnWatchTask: %v", err)
	}
	if len(res.scanned) != 1 || res.scanned[0] != "fetchedtx" {
		t.Fatalf("scanned = %v, want only the successfully fetched txid (a failed fetch must not be marked scanned)", res.scanned)
	}
	if res.spender != "" {
		t.Fatalf("spender = %q, want empty", res.spender)
	}
}

// TestOwnWatchScanRecordsAllFetched proves the bookkeeping stays complete
// when every fetch succeeds: scanned must carry all swept txids so the
// incremental seen-set still advances.
func TestOwnWatchScanRecordsAllFetched(t *testing.T) {
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	conn.mempoolTxids = []string{"txa", "txb"}
	tx := &coins.Tx{Version: 1}
	raw := hex.EncodeToString(tx.Serialize())
	conn.setRawTx("txa", raw)
	conn.setRawTx("txb", raw)

	res, err := runOwnWatchTask(conn, map[string]struct{}{}, [20]byte{}, "deptxid", 0, false)
	if err != nil {
		t.Fatalf("runOwnWatchTask: %v", err)
	}
	if len(res.scanned) != 2 || res.scanned[0] != "txa" || res.scanned[1] != "txb" {
		t.Fatalf("scanned = %v, want [txa txb]", res.scanned)
	}
}
