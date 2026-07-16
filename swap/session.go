package swap

import (
	"crypto/rand"
	"errors"

	"xbridge-go/coins"
	"xbridge-go/wallet"
)

// Session drives the deposit/refund (xbridgesession*) layer for a joined
// Transaction.
//
// FIDELITY NOTE: in C++ the Transaction::m_state enum's trSigned / trCommited
// are vestigial — grepping xbridgetransaction.cpp and xbridgesession.cpp shows
// they are never assigned (only trHold / trInitialized / trCreated / trFinished
// / trCancelled / trDropped are). Deposits instead GATE the two-confirmation
// progression: when both sides' deposits confirm, xbridgesession calls
// increaseStateCounter(trJoined, fromSource) for each member, advancing
// trJoined -> trHold. This port mirrors that exactly — the Session tracks
// deposit creation/confirmation on its own fields and advances the gate, and
// does NOT assign the unused trSigned/trCommited states.
type Session struct {
	T        *Transaction
	Role     Role
	LocalPub [33]byte
	OtherPub [33]byte

	Local *DepositSpec // local participant's deposit (nil until created)
	Other *DepositSpec // counterparty's deposit (nil until adopted)

	localDeposited bool
	otherDeposited bool
	localConfirmed bool
	otherConfirmed bool
}

// NewSession wraps a joined Transaction for the given local role. The
// Transaction must already be trJoined (TryJoin completed); the local and
// counterparty 33-byte compressed pubkeys identify the HTLC participants.
func NewSession(t *Transaction, role Role, localPub, otherPub [33]byte) *Session {
	return &Session{T: t, Role: role, LocalPub: localPub, OtherPub: otherPub}
}

// CreateLocalDeposit generates a fresh 33-byte secret and builds the local
// deposit spec (HTLC keyed to local+other pubkeys). Amount/currency are derived
// from the joined Transaction for the local role. The caller funds, signs and
// broadcasts the deposit tx, then reveals SecretHash to the counterparty via
// xbcTransactionInit.
func (s *Session) CreateLocalDeposit(lockTime uint32) (*DepositSpec, error) {
	if s.T.State != TrJoined {
		return nil, errors.New("swap: session requires a joined transaction")
	}
	var secret [33]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	amt, cur := s.localDepositAmount()
	s.Local = &DepositSpec{
		Currency:        cur,
		Amount:          amt,
		DepositorPub:    s.LocalPub,
		CounterpartyPub: s.OtherPub,
		Secret:          secret,
		LockTime:        lockTime,
	}
	s.localDeposited = true
	return s.Local, nil
}

// AdoptCounterparty records the counterparty's revealed deposit (pubkey +
// secretHash + lockTime from xbcTransactionInit), so the local node can build
// the matching claim/refund scripts and know the deposited amount/currency.
func (s *Session) AdoptCounterparty(secretHash [20]byte, lockTime uint32) {
	amt, cur := s.otherDepositAmount()
	s.Other = &DepositSpec{
		Currency:        cur,
		Amount:          amt,
		DepositorPub:    s.OtherPub,
		CounterpartyPub: s.LocalPub,
		Hash:            secretHash,
		LockTime:        lockTime,
	}
	s.otherDeposited = true
}

// BuildLocalDepositTx constructs the unsigned local deposit tx from funding UTXOs.
func (s *Session) BuildLocalDepositTx(c coins.Coin, funding []wallet.Utxo, change [20]byte, fee uint64) (*coins.Tx, error) {
	if s.Local == nil {
		return nil, errors.New("swap: local deposit not created")
	}
	return s.Local.BuildDepositTx(c, funding, change, fee)
}

// ConfirmLocalDeposit / ConfirmOtherDeposit record on-chain confirmation of each
// side's deposit and advance the progression gate once both are confirmed.
func (s *Session) ConfirmLocalDeposit() { s.localConfirmed = true; s.advance() }
func (s *Session) ConfirmOtherDeposit() { s.otherConfirmed = true; s.advance() }

// advance pushes the progression gate after a deposit confirms. C++'s
// xbridgesession drives the two-confirmation gate for each phase via
// increaseStateCounter: trJoined/trInitialized are advanced by marking the
// Source addresses, trHold/trCreated by the Dest addresses, and a phase flips
// only once BOTH members have marked it. Because the client observes each
// deposit confirmation once, we mark whichever side just confirmed for the
// current phase, and once BOTH deposits confirm we run the remaining gates so
// the swap reaches trFinished (mirroring C++ reaching a finished transaction
// once both deposits are on-chain).
func (s *Session) advance() {
	if s.T.State != TrJoined {
		return
	}
	// Mark whichever deposit(s) just confirmed for the Source phase.
	if s.localConfirmed {
		s.T.IncreaseStateCounter(TrJoined, s.localSource())
	}
	if s.otherConfirmed {
		s.T.IncreaseStateCounter(TrJoined, s.otherSource())
	}
	// Once both deposits confirm, drive through every remaining gate to a
	// finished swap. Each phase requires both members marked, alternating
	// Source (trJoined/trInitialized) and Dest (trHold/trCreated) address sets.
	if s.localConfirmed && s.otherConfirmed {
		for s.T.State != TrFinished {
			st := s.T.State
			switch st {
			case TrJoined:
				s.T.IncreaseStateCounter(TrJoined, s.localSource())
				s.T.IncreaseStateCounter(TrJoined, s.otherSource())
			case TrHold:
				s.T.IncreaseStateCounter(TrHold, s.localDest())
				s.T.IncreaseStateCounter(TrHold, s.otherDest())
			case TrInitialized:
				s.T.IncreaseStateCounter(TrInitialized, s.localSource())
				s.T.IncreaseStateCounter(TrInitialized, s.otherSource())
			case TrCreated:
				s.T.IncreaseStateCounter(TrCreated, s.localDest())
				s.T.IncreaseStateCounter(TrCreated, s.otherDest())
			default:
				return
			}
		}
	}
}

func (s *Session) localSource() Addr {
	if s.Role == RoleMaker {
		return s.T.A.Source
	}
	return s.T.B.Source
}

func (s *Session) otherSource() Addr {
	if s.Role == RoleMaker {
		return s.T.B.Source
	}
	return s.T.A.Source
}

func (s *Session) localDest() Addr {
	if s.Role == RoleMaker {
		return s.T.A.Dest
	}
	return s.T.B.Dest
}

func (s *Session) otherDest() Addr {
	if s.Role == RoleMaker {
		return s.T.B.Dest
	}
	return s.T.A.Dest
}

// localDepositAmount returns what the local node locks: the maker locks
// SourceAmount/SourceCurrency; the taker locks DestAmount/DestCurrency.
func (s *Session) localDepositAmount() (uint64, string) {
	if s.Role == RoleMaker {
		return s.T.SourceAmount, s.T.SourceCurrency
	}
	return s.T.DestAmount, s.T.DestCurrency
}

// otherDepositAmount returns what the counterparty locks (the mirror of the
// local side).
func (s *Session) otherDepositAmount() (uint64, string) {
	if s.Role == RoleMaker {
		return s.T.DestAmount, s.T.DestCurrency
	}
	return s.T.SourceAmount, s.T.SourceCurrency
}
