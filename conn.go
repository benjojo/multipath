package multipath

import (
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type mpConn struct {
	cid           connectionID
	lastFN        uint64
	subflows      []*subflow
	muSubflows    sync.RWMutex
	recvQueue     *receiveQueue
	closed        uint32 // 1 == true, 0 == false
	tryRetransmit chan bool

	pendingAckMap map[uint64]*pendingAck
	pendingAckMu  *sync.RWMutex
}

func newMPConn(cid connectionID) *mpConn {
	return &mpConn{cid: cid,
		lastFN:        minFrameNumber - 1,
		recvQueue:     newReceiveQueue(recieveQueueLength),
		tryRetransmit: make(chan bool, 1),
		pendingAckMap: make(map[uint64]*pendingAck),
		pendingAckMu:  &sync.RWMutex{},
	}
}
func (bc *mpConn) Read(b []byte) (n int, err error) {
	return bc.recvQueue.read(b)
}

func (bc *mpConn) Write(b []byte) (n int, err error) {
	frame := composeFrame(atomic.AddUint64(&bc.lastFN, 1), b)

	for _, sf := range bc.sortedSubflows() {
		select {
		case sf.sendQueue <- frame:
			return len(b), nil
		default:
		}
	}

	for _, sf := range bc.sortedSubflows() {
		sf.sendQueue <- frame
		return len(b), nil
	}
	return 0, ErrClosed
}

func (bc *mpConn) Close() error {
	bc.close()
	for _, sf := range bc.sortedSubflows() {
		sf.close()
	}
	return nil
}

func (bc *mpConn) close() {
	atomic.StoreUint32(&bc.closed, 1)
	bc.recvQueue.close()
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "multipath" }
func (fakeAddr) String() string  { return "multipath" }

func (bc *mpConn) LocalAddr() net.Addr {
	return fakeAddr{}
}

func (bc *mpConn) RemoteAddr() net.Addr {
	return fakeAddr{}
}

func (bc *mpConn) SetDeadline(t time.Time) error {
	bc.SetReadDeadline(t)
	return bc.SetWriteDeadline(t)
}

func (bc *mpConn) SetReadDeadline(t time.Time) error {
	bc.recvQueue.setReadDeadline(t)
	return nil
}

func (bc *mpConn) SetWriteDeadline(t time.Time) error {
	bc.muSubflows.RLock()
	defer bc.muSubflows.RUnlock()
	for _, sf := range bc.subflows {
		if err := sf.conn.SetWriteDeadline(t); err != nil {
			return err
		}
	}
	return nil
}

func (bc *mpConn) retransmit(frame *sendFrame) {
	if atomic.LoadUint64(&frame.beingRetransmitted) == 1 {
		return
	}
	atomic.StoreUint64(&frame.beingRetransmitted, 1)
	defer func() {
		atomic.StoreUint64(&frame.beingRetransmitted, 0)
	}()

	subflows := bc.sortedSubflows()
	// ticker := time.NewTimer(time.Minute)

	if frame.retransmissions > 4 {
		frame.isDataFrame() // debugging point, sorry this can be removed in prod
	}

	alreadyTransmittedOnAllSubflows := false
	for {
		abort := false
		if bc.closed == 1 {
			return
		}

		for _, sf := range subflows {
			select {
			case <-sf.chClose:
				continue
			case sf.sendQueue <- frame:
				frame.retransmissions++
				log.Tracef("retransmitted frame %d via %s", frame.fn, sf.to)
				return
			default:
			}
		}
		if abort {
			break
		}
		<-bc.tryRetransmit
	}

	if !alreadyTransmittedOnAllSubflows {
		log.Debugf("frame %d is being retransmitted on all subflows of %x", frame.fn, bc.cid)
	}

	// frame.release() // Commented out since it's not clear that this is needed anymore.
	return
}

func (bc *mpConn) sortedSubflows() []*subflow {
	bc.muSubflows.RLock()
	subflows := make([]*subflow, len(bc.subflows))
	copy(subflows, bc.subflows)
	bc.muSubflows.RUnlock()
	sort.Slice(subflows, func(i, j int) bool {
		return subflows[i].getRTT() < subflows[j].getRTT()
	})
	return subflows
}

func (bc *mpConn) add(to string, c net.Conn, clientSide bool, probeStart time.Time, tracker StatsTracker) {
	bc.muSubflows.Lock()
	defer bc.muSubflows.Unlock()
	bc.subflows = append(bc.subflows, startSubflow(to, c, bc, clientSide, probeStart, tracker))
}

func (bc *mpConn) remove(theSubflow *subflow) {
	bc.muSubflows.Lock()
	var remains []*subflow
	for _, sf := range bc.subflows {
		if sf != theSubflow {
			remains = append(remains, sf)
		}
	}
	bc.subflows = remains
	left := len(remains)
	bc.muSubflows.Unlock()
	if left == 0 {
		bc.close()
	}
}

func (bc *mpConn) retransmitLoop() {
	evalTick := time.NewTicker(time.Millisecond * 100)
	for {
		select {
		case <-evalTick.C:
		}
		if bc.closed == 1 {
			return
		}

		bc.pendingAckMu.RLock()
		RetransmitFrames := make([]pendingAck, 0)
		for fn, frame := range bc.pendingAckMap {
			if time.Since(frame.sentAt) > frame.outboundSf.retransTimer() {
				if bc.pendingAckMap[fn] != nil {
					RetransmitFrames = append(RetransmitFrames, *frame)
				}
			}
		}
		bc.pendingAckMu.RUnlock()

		sort.Slice(RetransmitFrames, func(i, j int) bool {
			return RetransmitFrames[i].fn < RetransmitFrames[j].fn
		})

		for _, frame := range RetransmitFrames {
			sendframe := frame.framePtr
			if bc.isPendingAck(frame.fn) {
				// No ack means the subflow fails or has a longer RTT
				// log.Errorf("Retransmitting! %#v", frame.fn)
				if sendframe.beingRetransmitted == 0 {
					go bc.retransmit(sendframe)
				}
			} else {
				// It is ok to release buffer here as the frame will never
				// be retransmitted again.
				sendframe.release()
				bc.pendingAckMu.Lock()
				delete(bc.pendingAckMap, frame.fn)
				bc.pendingAckMu.Unlock()
			}
		}

	}
}

func (bc *mpConn) isPendingAck(fn uint64) bool {
	if fn > minFrameNumber {
		bc.pendingAckMu.RLock()
		defer bc.pendingAckMu.RUnlock()
		return bc.pendingAckMap[fn] != nil
	}
	return false

}
