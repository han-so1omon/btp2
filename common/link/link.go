package link

import (
	"fmt"
	"math/rand"
	"sync"

	"github.com/icon-project/btp2/chain"
	"github.com/icon-project/btp2/common/errors"
	"github.com/icon-project/btp2/common/log"
	"github.com/icon-project/btp2/common/types"
)

type RelayState int

const (
	RUNNING = iota
	PENDING
)

type relayMessage struct {
	id            int
	bls           *types.BMCLinkStatus
	bpHeight      int64
	message       []byte
	rmis          []RelayMessageItem
	sendingStatus bool
}

func (r *relayMessage) Id() int {
	return r.id
}

func (r *relayMessage) Bytes() []byte {
	return r.message
}

func (r *relayMessage) Size() int64 {
	return int64(len(r.message))
}

func (r *relayMessage) BMCLinkStatus() *types.BMCLinkStatus {
	return r.bls
}

func (r *relayMessage) BpHeight() int64 {
	return r.bpHeight
}

func (r *relayMessage) RelayMessageItems() []RelayMessageItem {
	return r.rmis
}

type relayMessageItem struct {
	rmis [][]RelayMessageItem
	size int64
}

type receiveStatus struct {
	height int64
	seq    int64
	msgCnt int64
}

func NewReceiveStatus(rs ReceiveStatus, msgCnt int64) *receiveStatus {
	return &receiveStatus{
		height: rs.Height(),
		seq:    rs.Seq(),
		msgCnt: msgCnt,
	}
}

type Link struct {
	r          Receiver
	s          types.Sender
	l          log.Logger
	mtx        sync.RWMutex
	src        types.BtpAddress
	dst        types.BtpAddress
	rmsMtx     sync.RWMutex
	rms        []*relayMessage
	rss        []*receiveStatus
	rmi        *relayMessageItem
	limitSize  int64
	cfg        *chain.Config //TODO config refactoring
	bls        *types.BMCLinkStatus
	blsChannel chan *types.BMCLinkStatus
	relayState RelayState
}

func NewLink(cfg *chain.Config, r Receiver, l log.Logger) types.Link {
	link := &Link{
		src: cfg.Src.Address,
		dst: cfg.Dst.Address,
		l:   l.WithFields(log.Fields{log.FieldKeyChain: fmt.Sprintf("%s", cfg.Dst.Address.NetworkID())}),
		cfg: cfg,
		r:   r,
		rms: make([]*relayMessage, 0),
		rss: make([]*receiveStatus, 0),
		rmi: &relayMessageItem{
			rmis: make([][]RelayMessageItem, 0),
			size: 0,
		},
		blsChannel: make(chan *types.BMCLinkStatus),
		relayState: RUNNING,
	}
	link.rmi.rmis = append(link.rmi.rmis, make([]RelayMessageItem, 0))
	return link
}

func (l *Link) Start(sender types.Sender) error {
	l.s = sender
	errCh := make(chan error)
	l.senderChannel(errCh)

	bls, err := l.s.GetStatus()
	if err != nil {
		return err
	}

	l.bls = bls

	l.receiverChannel(errCh)

	l.r.FinalizedStatus(l.blsChannel)

	for {
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *Link) Stop() {
	l.s.Stop()
	l.r.Stop()
}

func (l *Link) receiverChannel(errCh chan error) {
	once := new(sync.Once)
	go func() {
		rsc, err := l.r.Start(l.bls)
		for {
			select {
			case rs := <-rsc:
				switch t := rs.(type) {
				case ReceiveStatus:
					var r *receiveStatus
					if len(l.rss) == 0 {
						r = NewReceiveStatus(rs, rs.Seq())
						l.l.Debugf("ReceiveStatus height:%d, seq:%d, msgCnt:%d", r.height, r.seq, r.msgCnt)
					} else {
						r = NewReceiveStatus(rs, l.rss[len(l.rss)-1].seq-rs.Seq())
						l.l.Debugf("ReceiveStatus height:%d, seq:%d, msgCnt:%d", r.height, r.seq, r.msgCnt)
					}
					l.rss = append(l.rss, r)

					once.Do(func() {
						if err = l.handleUndeliveredRelayMessage(); err != nil {
							errCh <- err
						}

						if err = l.HandleRelayMessage(); err != nil {
							errCh <- err
						}
						l.relayState = PENDING
					})

					if err = l.HandleRelayMessage(); err != nil {
						errCh <- err
					}
				case error:
					errCh <- t
				}
			}
		}

		select {
		case errCh <- err:
		default:
		}
	}()
}

func (l *Link) senderChannel(errCh chan error) {
	go func() {
		l.limitSize = int64(l.s.TxSizeLimit()) - l.s.GetMarginForLimit()
		rcc, err := l.s.Start()

		for {
			select {
			case rc := <-rcc:
				err := l.result(rc)
				errCh <- err
			}
		}

		select {
		case errCh <- err:
		default:
		}
	}()

}

func (l *Link) clearRelayMessage(bls *types.BMCLinkStatus) {
	for i, rm := range l.rms {
		if rm.bls.Verifier.Height <= bls.Verifier.Height && rm.bls.RxSeq <= bls.RxSeq {
			l.rms = l.rms[i+1:]
			break
		}
	}
}

func (l *Link) clearReceiveStatus(bls *types.BMCLinkStatus) {
	for i, rs := range l.rss {
		if rs.height <= bls.Verifier.Height && rs.seq <= bls.RxSeq {
			l.rss = l.rss[i+1:]
			break
		}
	}
}

func (l *Link) buildRelayMessage() error {
	if len(l.rmi.rmis) == 0 {
		l.resetRelayMessageItem()
	}

	//Get Block
	bus, err := l.buildBlockUpdates(l.bls)
	if err != nil {
		return err
	}

	if len(bus) != 0 {
		for _, bu := range bus {
			l.rmi.rmis[len(l.rmi.rmis)-1] = append(l.rmi.rmis[len(l.rmi.rmis)-1], bu)
			l.rmi.size += bu.Len()
			err := bu.UpdateBMCLinkStatus(l.bls)
			if err != nil {
				return err
			}
			//TODO if only block updates are delivered without a message
			//rs := l.searchReceiveStatusForHeight(l.bls.Verifier.Height)
			//if rs.msgCnt != 0 {}
			if err = l.buildProof(l.bls, bu); err != nil {
				return err
			}

			if err = l.appendRelayMessage(l.bls); err != nil {
				return err
			}
		}
	}

	return nil
}

func (l *Link) sendRelayMessage() error {
	for _, rm := range l.rms {
		if rm.sendingStatus == false {

			_, err := l.s.Relay(rm)
			if err != nil {
				if errors.InvalidStateError.Equals(err) {
					l.relayState = PENDING
					return nil
				} else {
					return err
				}
			} else {
				rm.sendingStatus = true
			}
		}
	}
	return nil
}

func (l *Link) appendRelayMessage(bls *types.BMCLinkStatus) error {
	for _, rmi := range l.rmi.rmis {
		m, err := l.r.BuildRelayMessage(rmi)
		if err != nil {
			return err
		}

		rm := &relayMessage{
			id:       rand.Int(),
			bls:      bls,
			bpHeight: l.r.GetHeightForSeq(bls.RxSeq),
			message:  m,
			rmis:     rmi,
		}

		rm.sendingStatus = false
		l.rms = append(l.rms, rm)
	}

	l.rmi.rmis = l.rmi.rmis[:0]
	l.resetRelayMessageItem()

	return nil
}

func (l *Link) HandleRelayMessage() error {
	l.rmsMtx.Lock()
	defer l.rmsMtx.Unlock()
	if l.relayState == RUNNING {
		if err := l.sendRelayMessage(); err != nil {
			return err
		}

		for true {
			if l.relayState == RUNNING &&
				len(l.rss) != 0 &&
				l.bls.Verifier.Height < l.rss[len(l.rss)-1].height {
				l.buildRelayMessage()
				l.sendRelayMessage()
			} else {
				break
			}
		}
	}
	return nil
}

func (l *Link) buildBlockUpdates(bs *types.BMCLinkStatus) ([]BlockUpdate, error) {
	for {
		bus, err := l.r.BuildBlockUpdate(bs, l.limitSize-l.rmi.size)
		if err != nil {
			return nil, err
		}
		if len(bus) != 0 {
			return bus, nil
		}
	}
}

func (l *Link) handleUndeliveredRelayMessage() error {
	lastSeq := l.bls.RxSeq
	for {
		h := l.r.GetHeightForSeq(lastSeq)
		if h == 0 {
			break
		}
		if h == l.bls.Verifier.Height {
			mp, err := l.r.BuildMessageProof(l.bls, l.limitSize-l.rmi.size)
			if err != nil {
				return err
			}

			if mp == nil {
				break
			}

			if mp.Len() != 0 || l.bls.RxSeq < mp.LastSeqNum() {
				l.rmi.rmis[len(l.rmi.rmis)-1] = append(l.rmi.rmis[len(l.rmi.rmis)-1], mp)
				l.rmi.size += mp.Len()
			}
			break
		} else if h < l.bls.Verifier.Height {
			err := l.buildProof(l.bls, nil)
			if err != nil {
				return err
			}
		} else {
			break
		}
	}
	if l.rmi.size > 0 {
		l.appendRelayMessage(l.bls)
	}
	return nil
}

func (l *Link) buildProof(bls *types.BMCLinkStatus, bu BlockUpdate) error {
	rs := l.getReceiveStatusForHeight(bls)
	if rs == nil {
		return nil
	}
	for {
		//TODO refactoring
		if rs.seq <= bls.RxSeq {
			break
		}
		if l.isOverLimit(l.rmi.size) {
			l.appendRelayMessage(bls)
			if err := l.buildBlockProof(bls); err != nil {
				return err
			}
		} else {
			if bu == nil || bu.ProofHeight() == -1 {
				if err := l.buildBlockProof(bls); err != nil {
					return err
				}
			}
		}
		if err := l.buildMessageProof(bls); err != nil {
			return err
		}
	}
	return nil
}

func (l *Link) getReceiveStatusForHeight(bls *types.BMCLinkStatus) *receiveStatus {
	for _, rs := range l.rss {
		if rs.height == bls.Verifier.Height {
			return rs
		}
	}
	return nil
}

func (l *Link) buildMessageProof(bls *types.BMCLinkStatus) error {
	mp, err := l.r.BuildMessageProof(bls, l.limitSize-l.rmi.size)
	if err != nil {
		return err
	}
	if mp != nil {
		l.rmi.rmis[len(l.rmi.rmis)-1] = append(l.rmi.rmis[len(l.rmi.rmis)-1], mp)
		l.rmi.size += mp.Len()
		if err := mp.UpdateBMCLinkStatus(bls); err != nil {
			return err
		}
	}
	return nil
}

func (l *Link) buildBlockProof(bls *types.BMCLinkStatus) error {
	h := l.r.GetHeightForSeq(bls.RxSeq)
	bf, err := l.r.BuildBlockProof(bls, h)
	if err != nil {
		return err
	}

	if bf != nil {
		l.rmi.rmis[len(l.rmi.rmis)-1] = append(l.rmi.rmis[len(l.rmi.rmis)-1], bf)
		l.rmi.size += bf.Len()
		if err := bf.UpdateBMCLinkStatus(bls); err != nil {
			return err
		}

	}
	return nil
}

func (l *Link) removeRelayMessage(id int) {
	index := 0
	for i, rm := range l.rms {
		if rm.id == id {
			index = i
		}
	}

	l.l.Debugf("remove relay message h:%d seq:%d", l.rms[index].BMCLinkStatus().Verifier.Height,
		l.rms[index].BMCLinkStatus().RxSeq)
	l.rms = l.rms[index:]

}

func (l *Link) updateBlockProof(id int) error {
	rm := l.searchRelayMessage(id)
	l.buildProof(rm.bls, nil)
	l.appendRelayMessage(rm.bls)
	return nil
}

func (l *Link) searchReceiveStatusForHeight(height int64) *receiveStatus {
	for _, rs := range l.rss {
		if rs.height == height {
			return rs
		}
	}
	return nil
}

func (l *Link) searchRelayMessage(id int) *relayMessage {
	for _, rm := range l.rms {
		if rm.Id() == id {
			return rm
		}
	}
	return nil
}

func (l *Link) isOverLimit(size int64) bool {
	if int64(l.s.TxSizeLimit()) < size {
		return true
	}
	return false
}

func (l *Link) resetRelayMessageItem() {
	l.rmi.rmis = append(l.rmi.rmis, make([]RelayMessageItem, 0))
	l.rmi.size = 0
}

func (l *Link) successRelayMessage(id int) error {
	rm := l.searchRelayMessage(id)
	l.clearRelayMessage(rm.BMCLinkStatus())
	l.clearReceiveStatus(rm.BMCLinkStatus())

	l.relayState = RUNNING

	err := l.HandleRelayMessage()
	if err != nil {
		return err
	}
	l.blsChannel <- rm.BMCLinkStatus()
	return nil
}

func (l *Link) result(rr *types.RelayResult) error {
	switch rr.Err {
	case errors.SUCCESS:
		if l.cfg.Dst.LatestResult == true {
			l.successRelayMessage(rr.Id)
		} else {
			if rr.Finalized == true {
				l.successRelayMessage(rr.Id)
			}
		}
	case errors.BMVUnknown:
		l.l.Panicf("BMVUnknown Revert : ErrorCoder:%+v", rr.Err)
	case errors.BMVNotVerifiable:
		if rr.Finalized != true {
			l.relayState = PENDING
		} else {
			bls, err := l.s.GetStatus()
			if err != nil {
				return err
			}
			l.bls = bls
			l.clearRelayMessage(l.bls) // TODO refactoring
			l.relayState = RUNNING
			l.HandleRelayMessage()
		}
	case errors.BMVAlreadyVerified:
		//TODO Error handling required on Finalized
		l.removeRelayMessage(rr.Id)
	case errors.BMVRevertInvalidBlockWitnessOld:
		//TODO Error handling required on Finalized
		l.updateBlockProof(rr.Id)
	default:
		l.l.Panicf("fail to GetResult RelayMessage ID:%v ErrorCoder:%+v",
			rr.Id, rr.Err)
	}
	return nil
}
