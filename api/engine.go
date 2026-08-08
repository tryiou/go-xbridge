package api

import (
	"errors"
	"fmt"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/proto"
)

// engineWorkers is the size of the wallet-RPC worker pool. Refund sweeps (and,
// from stage 4, the deposit/claim two-phase handshake) offload wallet I/O here
// so the engine goroutine never blocks on a slow RPC.
const engineWorkers = 4

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
// functions directly. Public wrappers (dispatchSwap, onRemoteCancel,
// onRemoteReject, BroadcastRefund, checkRefunds) are the only submit callers.
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
	n.wg.Add(4) // reader, engine, blockLoop, statusLoop
	go n.readerLoop()
	go n.engineLoop()
	go n.blockLoop()
	go n.statusLoop()
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
	for {
		select {
		case <-n.stop:
			return
		default:
		}
		pkt, peer, err := n.conn.ReadPacket()
		if err != nil {
			if !errors.Is(err, lastErr) {
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
		select {
		case n.packets <- in:
		case <-n.stop:
			return
		default:
			xlog.Debug("packet dropped, engine busy", "command", pkt.Command.String())
		}
	}
}

// engineLoop owns all mutable state (sessions, refund state, persist cadence)
// and processes, in order: handler commands, decoded packets, worker results,
// and the refund/persist ticker. It is the single writer of n.sessions and the
// persist path.
func (n *Node) engineLoop() {
	defer n.wg.Done()
	defer close(n.tasks) // hygiene; workers also exit on n.stop
	t := time.NewTicker(refundCheckInterval)
	defer t.Stop()
	for {
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
			n.tickCount++
			// Fund-safety sweep (was refundWatcher): auto-broadcast any
			// pre-signed refund whose deposit lockTime has passed.
			n.safeRun(func() { n.checkRefunds() })
			// Mirror C++ saveOrders cadence: flush local swap state to disk
			// periodically so a crash loses at most a few minutes of progress.
			if n.tickCount%4 == 0 {
				n.safeRun(func() { n.persist() })
			}
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
	select {
	case <-n.stop:
	default:
		close(n.stop)
	}
	n.engineRunning.Store(false)
	var cerr error
	if n.conn != nil {
		cerr = n.conn.Close()
	}
	n.wg.Wait()
	return cerr
}
