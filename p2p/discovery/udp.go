/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package discovery

import (
	"container/list"
	"net"
	"time"

	"github.com/seeleteam/go-seele/common"
	_ "github.com/seeleteam/go-seele/common/hexutil"
)

const (
	responseTimeout = 10 * time.Second

	pingpongInterval  = 15 * time.Second // sleep between ping pong, must big than response time out
	discoveryInterval = 20 * time.Second // sleep between discovery, must big than response time out
)

type udp struct {
	conn  *net.UDPConn
	self  *Node
	table *Table

	db        *Database
	localAddr *net.UDPAddr

	gotReply   chan *reply
	addPending chan *pending
	writer     chan *send
}

type pending struct {
	from *Node
	code msgType

	deadline time.Time

	callback func(resp interface{}, addr *net.UDPAddr) (done bool)

	errorCallBack func()
}

type send struct {
	buff []byte
	to   *Node
	code msgType
}

type reply struct {
	from *Node
	code msgType

	err bool // got error when send msg

	data interface{}
}

func newUDP(id common.Address, addr *net.UDPAddr) *udp {
	transport := &udp{
		conn:      getUDPConn(addr),
		table:     newTable(id, addr),
		self:      NewNodeWithAddr(id, addr),
		localAddr: addr,

		db: NewDatabase(),

		gotReply:   make(chan *reply, 1),
		addPending: make(chan *pending, 1),
		writer:     make(chan *send, 1),
	}

	return transport
}

func (u *udp) sendMsg(t msgType, msg interface{}, to *Node) {
	encoding, err := common.Serialize(msg)
	if err != nil {
		log.Info(err.Error())
		return
	}

	buff := generateBuff(t, encoding)
	s := &send{
		buff: buff,
		to:   to,
		code: t,
	}
	u.writer <- s
}

func sendMsg(buff []byte, conn *net.UDPConn, to *net.UDPAddr) bool {
	//log.Debug("buff length:", len(buff))
	n, err := conn.WriteToUDP(buff, to)
	if err != nil {
		log.Info("send msg failed:%s", err.Error())
		return false
	}

	if n != len(buff) {
		log.Error("send msg failed, expected length: %d, actuall length: %d", len(buff), n)
		return false
	}

	return true
}

func (u *udp) sendLoop() {
	for {
		select {
		case s := <-u.writer:
			//log.Debug("send msg to: %d", s.to.Port)
			success := sendMsg(s.buff, u.conn, s.to.GetUDPAddr())
			if !success {
				r := &reply{
					from: s.to,
					code: s.code,
					err:  true,
				}

				u.gotReply <- r
			}
		}
	}
}

func (u *udp) handleMsg(from *net.UDPAddr, data []byte) {
	if len(data) > 0 {
		code := byteToMsgType(data[0])

		//log.Debug("msg type: %d", code)
		switch code {
		case pingMsgType:
			msg := &ping{}
			err := common.Deserialize(data[1:], &msg)
			if err != nil {
				log.Info(err.Error())
				return
			}

			// response ping
			msg.handle(u, from)
		case pongMsgType:
			msg := &pong{}
			err := common.Deserialize(data[1:], &msg)
			if err != nil {
				log.Info(err.Error())
				return
			}

			r := &reply{
				from: NewNodeWithAddr(msg.SelfID, from),
				code: code,
				data: msg,
				err:  false,
			}

			u.gotReply <- r
		case findNodeMsgType:
			msg := &findNode{}

			err := common.Deserialize(data[1:], &msg)
			if err != nil {
				log.Info(err.Error())
				return
			}

			//response find
			msg.handle(u, from)
		case neighborsMsgType:
			msg := &neighbors{}
			err := common.Deserialize(data[1:], &msg)
			if err != nil {
				log.Info(err.Error())
				return
			}

			r := &reply{
				from: NewNodeWithAddr(msg.SelfID, from),
				code: code,
				data: msg,
				err:  false,
			}

			u.gotReply <- r
		default:
			log.Error("unknown code %d", code)
		}
	} else {
		log.Info("wrong length")
	}
}

func (u *udp) readLoop() {
	for {
		data := make([]byte, 1024)
		n, remoteAddr, err := u.conn.ReadFromUDP(data)
		if err != nil {
			log.Info(err.Error())
		}

		//log.Info("get msg from: %d", remoteAddr.Port)

		data = data[:n]
		u.handleMsg(remoteAddr, data)
	}
}

func (u *udp) loopReply() {
	pendingList := list.New()

	var timeout = time.NewTimer(0)
	<-timeout.C
	defer timeout.Stop()

	resetTimer := func() {
		minTime := responseTimeout
		now := time.Now()
		for el := pendingList.Front(); el != nil; el = el.Next() {
			p := el.Value.(*pending)
			duration := p.deadline.Sub(now)
			if duration < 0 {
			} else {
				if duration < minTime {
					minTime = duration
				}
			}
		}

		// if there is no pending request, stop timer
		if pendingList.Len() == 0 {
			timeout.Stop()
		} else {
			timeout.Reset(minTime)
		}
	}

	for {
		resetTimer()

		select {
		case r := <-u.gotReply:
			for el := pendingList.Front(); el != nil; el = el.Next() {
				p := el.Value.(*pending)

				if p.from.ID == r.from.ID && p.code == r.code {
					if r.err {
						p.errorCallBack()
						pendingList.Remove(el)
					} else {
						if p.callback(r.data, r.from.GetUDPAddr()) {
							pendingList.Remove(el)
						}
					}

					break
				}
			}
		case p := <-u.addPending:
			p.deadline = time.Now().Add(responseTimeout)
			pendingList.PushBack(p)
		case <-timeout.C:
			for el := pendingList.Front(); el != nil; el = el.Next() {
				p := el.Value.(*pending)
				if p.deadline.Sub(time.Now()) <= 0 {
					log.Debug("time out %d", p.code)
					p.errorCallBack()
					pendingList.Remove(el)
				}
			}

			resetTimer()
		}
	}
}

func (u *udp) discovery() {
	for {
		id, err := common.GenerateRandomAddress()
		if err != nil {
			log.Error(err.Error())
			continue
		}

		nodes := u.table.findNodeForRequest(id.ToSha())

		//log.Debug("query id: %s", hexutil.BytesToHex(id.Bytes()))
		sendFindNodeRequest(u, nodes, *id)

		time.Sleep(discoveryInterval)
	}
}

func (u *udp) pingPongService() {
	for {
		copyMap := u.db.GetCopy()

		for _, value := range copyMap {
			p := &ping{
				Version: discoveryProtocolVersion,
				SelfID:  u.self.ID,

				to: value,
			}

			p.send(u)
			time.Sleep(pingpongInterval)
		}
	}
}

func (u *udp) StartServe() {
	go u.readLoop()
	go u.loopReply()
	go u.discovery()
	go u.pingPongService()
	go u.sendLoop()
}

func (u *udp) addNode(n *Node) {
	if n == nil || n.ID == u.self.ID {
		return
	}

	u.table.addNode(n)
	u.db.add(n)
	//log.Info("add node, total nodes:%d", u.db.size())
}

func (u *udp) deleteNode(sha *common.Hash) {
	selfSha := u.self.getSha()
	if *sha == *selfSha {
		return
	}

	u.table.deleteNode(sha)
	u.db.delete(sha)
	log.Info("delete node, total nodes:%d", u.db.size())
}
