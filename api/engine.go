package api

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// isReadTimeout reports whether err is a network timeout — the idle read
// deadline the p2p layer re-arms per frame (p2p idleReadTimeout, mirroring
// C++ TIMEOUT_INTERVAL, net.h:45 / net.cpp:1068). It matches both the wrapped
// os.ErrDeadlineExceeded and the net.Error.Timeout() form.
func isReadTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// engineWorkers is the size of the wallet-RPC worker pool. Refund sweeps (and,
// from stage 4, the deposit/claim two-phase handshake) offload wallet I/O here
// so the engine goroutine never blocks on a slow RPC.
const engineWorkers = 4

// hubBanThreshold is the misbehaviour score at which the DIRECT hub connection
// is dropped. Mirrors C++ -banscore (default 100); each malformed/undersized
// xbridge envelope scores +10 (C++ Misbehaving, net_processing.cpp:2874-2878).
// In discovery mode the PeerManager enforces its own per-peer penalties instead.
const hubBanThreshold = 100

// inboundPacket is a raw P2P packet plus the reader-computed values that
// replace re-verifying on the engine goroutine.
type inboundPacket struct {
	pkt   *proto.Packet
	peer  string
	snode string // hexEncode(pkt.Pubkey[:]) — computed by the reader
}

// engineCmd is a request/response command queued to the engine goroutine by a
// handler (or a wrapper method). run executes on the engine (or inline on the
// caller when the engine is not started); resp is closed by the engine after
// run completes, and is nil when the caller does not await the result.
type engineCmd struct {
	run  func()
	resp chan struct{}
}

// workTask is a unit of wallet-I/O offloaded to a worker goroutine. run must be
// self-contained: it captures every value it needs (cur, refundHex, lockTime,
// order id) at enqueue time and never touches a SwapSession/Node field. Only
// apply mutates state, and it runs on the engine goroutine.
type workTask struct {
	orderID string
	run     func() (any, error)
	apply   func(v any, err error)
}

// workResult carries a completed workTask back to the engine.
type workResult struct {
	task  workTask
	value any
	err   error
}

// submit queues run for the engine goroutine. When the engine is not running
// (tests, or a node that never started) run executes inline on the caller —
// the single-threaded behaviour the existing tests rely on. await selects for
// run's completion (resp closed); both the enqueue and the await bail out on
// n.stop so a shutting-down node can never hang a caller.
//
// Deadlock rule: code that runs ON the engine goroutine (command closures,
// handlePacket, task apply) must never call submit — it calls the internal
// functions directly (processSwap, handleRemoteCancel, handleRemoteReject,
// scanRefunds). Off-engine callers (RPC handlers, tests, BroadcastRefund) go
// through submit to marshal the same work onto the engine goroutine.
func (n *Node) submit(run func(), await bool) {
	if !n.engineRunning.Load() {
		run()
		return
	}
	cmd := engineCmd{run: run}
	if await {
		cmd.resp = make(chan struct{})
	}
	select {
	case n.cmds <- cmd:
	case <-n.stop:
		return
	}
	if await {
		select {
		case <-cmd.resp:
		case <-n.stop:
		}
	}
}

// start launches the engine goroutine and its satellites: the socket reader,
// the periodic status logger, the anti-replay block refresher, and the wallet
// worker pool. NewNode calls it once after the peer connection is wired; the
// engine then owns all mutable state (sessions, refund state, persist).
func (n *Node) start() {
	n.packets = make(chan inboundPacket, 256)
	n.cmds = make(chan engineCmd, 64)
	n.tasks = make(chan workTask, 16)
	n.results = make(chan workResult, engineWorkers)
	n.pendingRefunds = map[string]bool{}
	n.persistSignal = make(chan struct{}, 1)
	n.wg.Add(6) // reader, engine, blockLoop, statusLoop, wallet sweep, persist
	go n.readerLoop()
	go n.engineLoop()
	go n.blockLoop()
	go n.statusLoop()
	go n.sweepLoop()
	n.persistUp.Store(true)
	go n.persistLoop()
	n.startWorkers()
	n.engineRunning.Store(true)
}

// readerLoop is the read half of the former feed(): it reads packets off the
// wire, decodes + signature-verifies the bodies, and forwards raw packets to
// the engine. Doing the (blocking) socket read and the decode/verify here means
// a slow peer can no longer stall state processing, and malformed/forged
// packets are dropped before they reach the engine.
func (n *Node) readerLoop() {
	defer n.wg.Done()
	var lastErr error
	// Hub misbehaviour score. For a DIRECT hub connection, an
	// xbridge envelope that fails transport decode accumulates +10 — the analog
	// of C++'s Misbehaving +10 for a sub-min-size xbridge packet
	// (net_processing.cpp:2874-2878) — and the connection is dropped at the ban
	// threshold. Sized-but-undecodable packet BODIES are dropped without a
	// penalty (C++ DoS 0, xbridgesession.cpp:312), matching the discovery
	// path. In discovery mode the PeerManager enforces its own per-peer
	// penalties (readLoop), so the direct score applies only to *p2p.Conn.
	_, direct := n.conn.(*p2p.Conn)
	hubScore := 0
	for {
		select {
		case <-n.stop:
			return
		default:
		}
		pkt, peer, err := n.conn.ReadPacket()
		if err != nil {
			if direct && errors.Is(err, p2p.ErrMalformedXBridge) {
				hubScore += 10
				if hubScore >= hubBanThreshold {
					xlog.Error("hub banned: malformed xbridge envelope flood", "peer", peer)
					_ = n.conn.Close()
					return
				}
				xlog.Warn("hub misbehaving", "peer", peer, "score", hubScore, "err", err)
			} else if isReadTimeout(err) {
				// Idle read deadline (p2p idleReadTimeout, mirroring C++
				// TIMEOUT_INTERVAL disconnect, net.cpp:1068): the hub went
				// silent. Close the connection instead of retrying forever —
				// a permanent stall would otherwise spin this loop.
				xlog.Warn("hub idle timeout, disconnecting", "peer", peer, "err", err)
				_ = n.conn.Close()
				return
			} else if !errors.Is(err, lastErr) {
				xlog.Debug("peer read failed", "peer", peer, "err", err)
				lastErr = err
			}
			select {
			case <-n.stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		lastErr = nil
		if _, err := proto.DecodeBody(pkt.Command, pkt.Body); err != nil {
			// Sized-but-undecodable body: dropped, no penalty (C++ DoS 0,
			// xbridgesession.cpp:312).
			xlog.Warn("packet body decode skipped", "command", pkt.Command.String(), "err", err)
			continue
		}
		// All traders verify the snode's packet signature against the pubkey
		// in the packet header (C++ xbridgesession.cpp:736, verbatim). A bad
		// signature means the packet was not signed by the claiming servicenode,
		// so it is dropped regardless of command.
		if ok, _ := n.signer.Verify(pkt); !ok {
			xlog.Warn("bad snode packet signature", "command", pkt.Command.String(), "snode", hexEncode(pkt.Pubkey[:]), "peer", peer)
			continue
		}
		in := inboundPacket{pkt: pkt, peer: peer, snode: hexEncode(pkt.Pubkey[:])}
		xlog.Debug("packet received", "command", pkt.Command.String(), "snode", in.snode, "peer", peer)
		// Backpressure, not drop: a full packets channel parks the reader
		// until the engine drains, matching C++'s synchronous net thread
		// (onMessageReceived/onBroadcastReceived process in place,
		// xbridgeapp.cpp:645-723,763) and the discovery readLoop
		// (peer_manager.go:405-409). The only loss is the in-flight packet at
		// shutdown, counted by packetsDropped.
		select {
		case n.packets <- in:
		case <-n.stop:
			n.packetsDropped.Add(1)
			return
		}
	}
}

// engineLoop owns all mutable state (sessions, refund state, persist cadence)
// and processes handler commands, decoded packets, worker results, and the
// refund/persist ticker. A worker result ready at the start of an iteration is
// applied before commands/packets, so a session's await guard clears before a
// same-session packet is judged. It is the single writer of n.sessions and the
// persist path.
func (n *Node) engineLoop() {
	defer n.wg.Done()
	defer close(n.tasks) // hygiene; workers also exit on n.stop
	t := time.NewTicker(refundCheckInterval)
	defer t.Stop()
	te := time.NewTicker(expirySweepInterval)
	defer te.Stop()
	for {
		// Priority: a worker result already queued at iteration start is applied
		// before any packet or ticker is judged, so a session's await guard is
		// cleared and its response sent before the next hub packet for that
		// session is handled — C++ processes synchronously in place, so a
		// completed deposit/claim lands first (xbridgeapp.cpp:645-723;
		// xbridgesession.cpp:1893 builds the deposit inline). A result that
		// arrives while the engine is blocked in the main select below is served
		// by its n.results case, so nothing waits on a full results buffer.
		select {
		case r := <-n.results:
			n.safeRun(func() { r.task.apply(r.value, r.err) })
			continue
		default:
		}
		select {
		case cmd := <-n.cmds:
			n.safeRun(func() { cmd.run() })
			if cmd.resp != nil {
				close(cmd.resp)
			}
		case in := <-n.packets:
			n.safeRun(func() { n.handlePacket(in) })
		case r := <-n.results:
			n.safeRun(func() { r.task.apply(r.value, r.err) })
		case <-t.C:
			// Fund-safety sweep: auto-broadcast any pre-signed refund whose
			// deposit lockTime has passed. Engine-owned: workers do the wallet
			// I/O; the engine applies the results.
			n.safeRun(func() { n.scanRefunds() })
			// Lifecycle sweep: drop terminal sessions (finished swaps, orders
			// whose refund has been broadcast) so the live set stays bounded.
			n.safeRun(func() { n.pruneSessions() })
			// Mirror C++ saveOrders cadence: flush local swap state to disk
			// every 60 s so a crash loses at most a minute of progress
			// (xbridgeapp.cpp:3744, every 4th 15 s timer tick).
			n.safeRun(func() { n.persist() })
		case <-te.C:
			// Order-book expiry sweep (C++ checkAndEraseExpiredTransactions on
			// the 15 s timer): prune open orders past their TTL/deadline.
			n.safeRun(func() { n.pruneExpired() })
			// Keep open maker orders alive: re-post them to the hub so they
			// survive the 6-min PendingTTL (C++ checkAndRelayPendingOrders,
			// xbridgeapp.cpp:3241, every 240 s).
			n.safeRun(func() { n.rebroadcastOpenOrders() })
		case <-n.stop:
			return
		}
	}
}

// statusLoop periodically emits an aggregated network-status snapshot so an
// operator can see peer/SN health and the live token set at a glance (the
// per-packet Debug stream is too noisy for that).
func (n *Node) statusLoop() {
	defer n.wg.Done()
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-tick.C:
			n.logNetworkStatus()
		}
	}
}

// persistLoop writes the newest swap snapshot to disk in the background, so
// the engine goroutine never blocks on marshal or fsync (C++ saveOrders runs
// on the timer/worker threads, never the message thread, xbridgeapp.cpp:3744).
// Coalescing: each pass drains the latest slot, so a burst of engine persists
// collapses into the newest snapshot. On n.stop it flushes whatever is queued;
// Node.Close additionally flushes the latest slot after every goroutine has
// joined, so the last state is durable before Close returns even when a final
// engine command raced the stop.
func (n *Node) persistLoop() {
	defer n.wg.Done()
	for {
		select {
		case <-n.persistSignal:
			n.writeLatestPersist()
		case <-n.stop:
			n.writeLatestPersist()
			return
		}
	}
}

// writeLatestPersist durably writes the newest queued swap snapshot, if any,
// logging a failure without taking the engine down. The marshal + checksum and
// the disk write both run on the persistLoop goroutine (or the Close caller),
// never the engine.
func (n *Node) writeLatestPersist() {
	// The lock covers only the slot swap: the marshal below runs unlocked, so
	// the engine can keep publishing while a large book is being serialized.
	n.persistMu.Lock()
	job := n.persistLatest
	n.persistLatest = nil
	n.persistMu.Unlock()
	if job == nil {
		return
	}
	data, err := marshalSwapFile(job.swaps)
	if err != nil {
		xlog.Error("swap persist failed", "dir", filepath.Dir(job.path), "err", err)
		n.persistFailures.Add(1)
		return
	}
	// Retry a transient disk failure with bounded backoff before giving up, so a
	// brief write error does not silently drop the only durable copy of in-flight
	// swap state (a restart would then be unable to refund/claim — fund loss).
	// C++ App::saveOrders (xbridgeapp.cpp:3868-3898) writes the orders DB via
	// xdb.Write with no retry; this port adds bounded retry so a transient EIO is
	// survived rather than silently losing the only durable copy.
	var lastErr error
	for attempt := 0; attempt < persistWriteMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(persistWriteBackoff << (attempt - 1))
		}
		if err := writeSwaps(job.path, data); err != nil {
			lastErr = err
			continue
		}
		return
	}
	xlog.Error("swap persist failed after retries", "dir", filepath.Dir(job.path), "err", lastErr)
	n.persistFailures.Add(1)
}

// persistWrite* bound the retry of a durable swap-state write. Three attempts
// with exponential backoff (25ms, then 50ms) cover a transient disk stall
// without materially delaying the background flush; the final attempt has no
// leading sleep, so total backoff is at most 75ms.
const (
	persistWriteMaxAttempts = 3
	persistWriteBackoff     = 25 * time.Millisecond
)

// safeRun executes fn, logging (and swallowing) any panic so a single bad
// packet or command can never kill the engine goroutine (which would silently
// stop all state processing).
func (n *Node) safeRun(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			xlog.Error("engine recovered panic", "panic", r)
		}
	}()
	fn()
}

// startWorkers launches the wallet-RPC worker pool. Workers pull workTask off
// n.tasks, run the self-contained run closure, and post the result back to the
// engine via n.results (buffered to the worker count so a result send can never
// block once the engine has exited).
func (n *Node) startWorkers() {
	for i := 0; i < engineWorkers; i++ {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			for {
				select {
				case t, ok := <-n.tasks:
					if !ok {
						return
					}
					v, err := safeTaskRun(t)
					select {
					case n.results <- workResult{task: t, value: v, err: err}:
					case <-n.stop: // engine gone: never block on results
						return
					}
				case <-n.stop:
					return
				}
			}
		}()
	}
}

// safeTaskRun executes a worker task with panic recovery, turning any panic
// into an error result so a single bad wallet response cannot kill a worker.
func safeTaskRun(t workTask) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			xlog.Error("worker recovered panic", "panic", r)
			err = fmt.Errorf("api: worker panic: %v", r)
		}
	}()
	return t.run()
}

// Close stops the engine, reader, workers, and periodic loops, then closes the
// connection and waits for every goroutine to join. main.go drains in-flight
// HTTP handlers (httpSrv.Shutdown) before calling this, so no handler races
// the teardown.
func (n *Node) Close() error {
	n.stopOnce.Do(func() { close(n.stop) })
	n.engineRunning.Store(false)
	var cerr error
	if n.conn != nil {
		cerr = n.conn.Close()
	}
	n.wg.Wait()
	// The engine may have published one final persist from a command that raced
	// the stop-drain, so flush the latest slot once more after every goroutine
	// has exited (no further publishes can race this drain).
	n.writeLatestPersist()
	// Stop every Dedupe sweeper so a library-level Close leaks no goroutine
	// (the daemon's main.go FlushAll becomes a no-op here). A later Event on a
	// reused node restarts its own sweeper on demand.
	xlog.FlushAll()
	return cerr
}
