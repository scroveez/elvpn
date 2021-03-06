/*
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 * Author: Mahmoud Abdelsalam <scroveez@gmail.com>
 *
 */

package el

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/songgao/water"
	"github.com/songgao/water/waterutil"
)

// a udpPacket
type udpPacket struct {
	// client's addr
	addr *net.UDPAddr
	// data
	data []byte
	// channel
	channel int
}

type ElServer struct {
	// config
	cfg ElServerConfig
	// interface
	iface *water.Interface
	// subnet
	ipnet *net.IPNet
	// IP Pool
	ippool *elIPPool
	// client peers, key is the mac address, value is a ElPeer record
	peers map[uint64]*ElPeer

	// channel to put in packets read from udpsocket
	fromNet chan *udpPacket
	// channel to put packets to send through udpsocket
	toNet []chan *udpPacket
	// channel to put frames read from tun/tap device
	fromIface chan []byte
	// channel to put frames to send to tun/tap device
	toIface chan *ElPacket

	pktHandle map[byte](func(*udpPacket, *ElPacket))

	_lock        sync.RWMutex
	_chanBufSize int
}

func NewServer(cfg ElServerConfig) error {
	var err error
	logger.Debug("%v", cfg)

	cipher, err = newElCipher([]byte(cfg.Key))
	if err != nil {
		return err
	}

	if cfg.MTU != 0 {
		MTU = cfg.MTU
	}

	elServer := new(ElServer)
	elServer._chanBufSize = 2048
	elServer.fromNet = make(chan *udpPacket, elServer._chanBufSize)
	elServer.fromIface = make(chan []byte, elServer._chanBufSize)
	elServer.toIface = make(chan *ElPacket, elServer._chanBufSize)
	elServer.peers = make(map[uint64]*ElPeer)
	elServer.cfg = cfg
	elServer.toNet = make([]chan *udpPacket, (cfg.HopEnd - cfg.HopStart + 1))
	elServer.ippool = new(elIPPool)

	iface, err := newTun("")
	if err != nil {
		return err
	}
	elServer.iface = iface
	ip, subnet, err := net.ParseCIDR(cfg.Addr)
	err = setTunIP(iface, ip, subnet)
	if err != nil {
		return err
	}
	elServer.ipnet = &net.IPNet{ip, subnet.Mask}
	elServer.ippool.subnet = subnet

	if cfg.FixMSS {
		fixMSS(iface.Name(), true)
	}

	// traffic morpher
	switch cfg.MorphMethod {
	case "randsize":
		// m := newRandMorpher(MTU)
		// hopFrager = newHopFragmenter(m)
		// logger.Info("Using RandomSize Morpher")
		logger.Warning("Traffic Morphing is disabled in this version")
	default:
		logger.Info("No Traffic Morphing")
	}

	// forward device frames to socket and socket packets to device
	go elServer.forwardFrames()

	// go func() {
	//     defer hopServer.cleanUp()
	//     redirectPort(cfg.HopRange, cfg.Port)
	// }()
	go elServer.cleanUp()

	// serve for multiple ports
	for idx, port := 0, cfg.HopStart; port <= cfg.HopEnd; port++ {
		go elServer.listenAndServe(cfg.ListenAddr, fmt.Sprintf("%d", port), idx)
		idx++
	}

	go elServer.peerTimeoutWatcher()
	logger.Debug("Recieving iface frames")

	// Post Up
	if cfg.Up != "" {
		args := strings.Split(cfg.Up, " ")
		var cmd *exec.Cmd
		if len(args) == 1 {
			cmd = exec.Command(args[0])
		} else {
			cmd = exec.Command(args[0], args[1:]...)
		}
		logger.Info(cfg.Up)
		cmd.Run()
	}

	// handle interface

	go func() {
		for {
			hp := <-elServer.toIface
			// logger.Debug("New Net packet to device")
			_, err := iface.Write(hp.payload)
			// logger.Debug("n: %d, len: %d", n, len(hp.payload))
			if err != nil {
				logger.Error(err.Error())
				return
			}
		}
	}()

	buf := make([]byte, IFACE_BUFSIZE)
	for {
		n, err := iface.Read(buf)
		if err != nil {
			return err
		}

		hpbuf := make([]byte, n+HOP_HDR_LEN)
		copy(hpbuf[HOP_HDR_LEN:], buf[:n])
		elServer.fromIface <- hpbuf
	}

}

func (srv *ElServer) listenAndServe(addr string, port string, idx int) {
	port = addr + ":" + port
	udpAddr, err := net.ResolveUDPAddr("udp", port)
	if err != nil {
		logger.Error("Invalid port: %s", port)
		return
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		logger.Error("Failed to listen udp port %s: %s", port, err.Error())
		return
	}

	toNet := make(chan *udpPacket, srv._chanBufSize)

	go func() {
		defer srv._lock.Unlock()
		srv._lock.Lock()
		srv.toNet[idx] = toNet
		// logger.Debug("Listening on port %s", port)
	}()

	go func() {
		for {
			packet := <-toNet
			// logger.Debug("index: %d, port: %s", idx, port)
			// logger.Debug("client addr: %v", packet.addr)
			udpConn.WriteTo(packet.data, packet.addr)
		}
	}()

	for {
		var plen int
		packet := new(udpPacket)
		packet.channel = idx
		buf := make([]byte, IFACE_BUFSIZE)
		// logger.Debug("Recieving packet %s", port)
		plen, packet.addr, err = udpConn.ReadFromUDP(buf)
		// logger.Debug("New UDP Packet from: %v", packet.addr)

		packet.data = buf[:plen]
		if err != nil {
			logger.Error(err.Error())
			return
		}

		srv.fromNet <- packet
	}

}

func (srv *ElServer) forwardFrames() {

	// packet map
	srv.pktHandle = map[byte](func(*udpPacket, *ElPacket)){
		HOP_FLG_PSH:               srv.handleKnock,
		HOP_FLG_PSH | HOP_FLG_ACK: srv.handleHeartbeatAck,
		HOP_FLG_HSH:               srv.handleHandshake,
		HOP_FLG_HSH | HOP_FLG_ACK: srv.handleHandshakeAck,
		HOP_FLG_DAT:               srv.handleDataPacket,
		HOP_FLG_DAT | HOP_FLG_MFR: srv.handleDataPacket,
		HOP_FLG_FIN:               srv.handleFinish,
	}

	for {
		select {
		case pack := <-srv.fromIface:
			// logger.Debug("New iface Frame")
			// first byte is left for opcode
			frame := pack[HOP_HDR_LEN:]
			dest := waterutil.IPv4Destination(frame).To4()
			mkey := ip4_uint64(dest)

			// logger.Debug("ip dest: %v", dest)
			if hpeer, found := srv.peers[mkey]; found {
				srv.bufferToClient(hpeer, pack)
			} else {
				logger.Warning("client peer with key %d not found", mkey)
			}

		case packet := <-srv.fromNet:
			srv.handlePacket(packet)
		}

	}
}

func (srv *ElServer) handlePacket(packet *udpPacket) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleFunction failed: %v, packet addr:%v", err, packet.addr)
		}
	}()

	hPack, err := unpackElPacket(packet.data)
	if err == nil {
		logger.Debug("New UDP Packet [%v] from : %v", hPack.Flag, packet.addr)
		if handle_func, ok := srv.pktHandle[hPack.Flag]; ok {
			handle_func(packet, hPack)
		} else {
			logger.Error("Unkown flag: %x", hPack.Flag)
		}
	} else {
		logger.Error(err.Error())
	}
}

func (srv *ElServer) toClient(peer *ElPeer, flag byte, payload []byte, noise bool) {
	hp := new(ElPacket)
	hp.Seq = peer.Seq()
	hp.Flag = flag
	hp.payload = payload

	if addr, idx, ok := peer.addr(); ok {
		logger.Debug("peer: %v", addr)
		upacket := &udpPacket{addr, hp.Pack(), idx}
		srv.toNet[idx] <- upacket
	} else {
		logger.Debug("peer not found")
	}
}

func (srv *ElServer) bufferToClient(peer *ElPeer, buf []byte) {
	hp := new(ElPacket)
	hp.Flag = HOP_FLG_DAT
	hp.buf = buf
	hp.payload = buf[HOP_HDR_LEN:]
	hp.Seq = peer.Seq()

	if addr, idx, ok := peer.addr(); ok {
		upacket := &udpPacket{addr, hp.Pack(), idx}
		srv.toNet[idx] <- upacket
	}

	/*
	   if elFrager == nil {
	       // if no traffic morphing
	   } else {
	       // with traffic morphing
	       frame := buf[HOP_HDR_LEN:]
	       packets := elFrager.Fragmentate(peer, frame)
	       for _, hp := range(packets) {
	           if addr, idx, ok := peer.addr(); ok {
	               upacket := &udpPacket{addr, hp.Pack(), idx}
	               srv.toNet[idx] <- upacket
	           }
	       }
	   }
	*/
}

func (srv *ElServer) handleKnock(u *udpPacket, hp *ElPacket) {
	sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
	logger.Debug("port knock from client %v, sid: %d", u.addr, sid)
	sid = (sid << 32) & uint64(0xFFFFFFFF00000000)

	hpeer, ok := srv.peers[sid]
	if !ok {
		hpeer = newElPeer(sid, srv, u.addr, u.channel)
		srv.peers[sid] = hpeer
	} else {
		hpeer.insertAddr(u.addr, u.channel)
		if hpeer.state == HOP_STAT_WORKING {
			srv.toClient(hpeer, HOP_FLG_PSH|HOP_FLG_ACK, []byte{0}, true)
		}
	}

	hpeer.lastSeenTime = time.Now()
}

func (srv *ElServer) handleHeartbeatAck(u *udpPacket, hp *ElPacket) {
	sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
	sid = (sid << 32) & uint64(0xFFFFFFFF00000000)

	hpeer, ok := srv.peers[sid]
	if !ok {
		return
	}

	hpeer.lastSeenTime = time.Now()
}

func (srv *ElServer) handleHandshake(u *udpPacket, hp *ElPacket) {
	sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
	sid = (sid << 32) & uint64(0xFFFFFFFF00000000)
	logger.Debug("handshake from client %v, sid: %d", u.addr, sid)

	hpeer, ok := srv.peers[sid]
	if !ok {
		hpeer = newElPeer(sid, srv, u.addr, u.channel)
		srv.peers[sid] = hpeer
	} else {
		hpeer.insertAddr(u.addr, u.channel)
	}

	cltIP, err := srv.ippool.next()
	if err != nil {
		msg := fmt.Sprintf("%s", err.Error())
		srv.toClient(hpeer, HOP_FLG_HSH|HOP_FLG_FIN, []byte(msg), true)
		delete(srv.peers, sid)
	} else {
		hpeer.ip = cltIP.IP.To4()
		mask, _ := cltIP.Mask.Size()
		buf := bytes.NewBuffer(make([]byte, 0, 8))
		buf.WriteByte(HOP_PROTO_VERSION)
		buf.Write([]byte(hpeer.ip))
		buf.WriteByte(byte(mask))
		key := ip4_uint64(hpeer.ip)

		logger.Debug("assign address %s, route key %d", cltIP, key)
		srv.peers[key] = hpeer
		atomic.StoreInt32(&hpeer.state, HOP_STAT_HANDSHAKE)
		srv.toClient(hpeer, HOP_FLG_HSH|HOP_FLG_ACK, buf.Bytes(), true)
		hpeer.hsDone = make(chan struct{})
		go func() {
			for i := 0; i < 5; i++ {
				select {
				case <-hpeer.hsDone:
					hpeer.state = HOP_STAT_WORKING
					return
				case <-time.After(2 * time.Second):
					logger.Debug("Client Handshake Timeout")
					srv.toClient(hpeer, HOP_FLG_HSH|HOP_FLG_ACK, buf.Bytes(), true)
				}
			}
			// timeout,  kick
			srv.toClient(hpeer, HOP_FLG_HSH|HOP_FLG_FIN, []byte{}, true)
			srv.toClient(hpeer, HOP_FLG_HSH|HOP_FLG_FIN, []byte{}, true)
			srv.toClient(hpeer, HOP_FLG_HSH|HOP_FLG_FIN, []byte{}, true)

			srv.ippool.relase(hpeer.ip)
			delete(srv.peers, sid)
			delete(srv.peers, key)

		}()
	}

}

func (srv *ElServer) handleHandshakeAck(u *udpPacket, hp *ElPacket) {
	sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
	sid = (sid << 32) & uint64(0xFFFFFFFF00000000)
	hpeer, ok := srv.peers[sid]
	if !ok {
		return
	}
	logger.Debug("Client Handshake Done")
	logger.Info("Client %d Connected", sid)
	if ok = atomic.CompareAndSwapInt32(&hpeer.state, HOP_STAT_HANDSHAKE, HOP_STAT_WORKING); ok {
		hpeer.hsDone <- struct{}{}
	} else {
		logger.Warning("Invalid peer state: %v", hpeer.ip)
		srv.kickOutPeer(sid)
	}
}

func (srv *ElServer) handleDataPacket(u *udpPacket, hp *ElPacket) {
	sid := uint64(hp.Sid)
	sid = (sid << 32) & uint64(0xFFFFFFFF00000000)

	if hpeer, ok := srv.peers[sid]; ok && hpeer.state == HOP_STAT_WORKING {
		// logger.Debug("n peer addrs: %v", len(peer._addrs_lst))
		// peer.insertAddr(u.addr, u.channel)
		hpeer.recvBuffer.Push(hp)
		hpeer.lastSeenTime = time.Now()
	}
}

func (srv *ElServer) handleFinish(u *udpPacket, hp *ElPacket) {
	sid := uint64(binary.BigEndian.Uint32(hp.payload[:4]))
	sid = (sid << 32) & uint64(0xFFFFFFFF00000000)
	logger.Info("releasing client %v, sid: %d", u.addr, sid)

	srv.deletePeer(sid)
}

func (srv *ElServer) kickOutPeer(sid uint64) {
	hpeer, ok := srv.peers[sid]
	if !ok {
		return
	}
	srv.deletePeer(sid)
	srv.toClient(hpeer, HOP_FLG_FIN, []byte{}, false)
	srv.toClient(hpeer, HOP_FLG_FIN, []byte{}, false)
	srv.toClient(hpeer, HOP_FLG_FIN, []byte{}, false)
}

func (srv *ElServer) deletePeer(sid uint64) {
	hpeer, ok := srv.peers[sid]
	if !ok {
		return
	}

	key := ip4_uint64(hpeer.ip)
	srv.ippool.relase(hpeer.ip)

	delete(srv.peers, sid)
	delete(srv.peers, key)

	srv.toClient(hpeer, HOP_FLG_FIN|HOP_FLG_ACK, []byte{}, false)
	srv.toClient(hpeer, HOP_FLG_FIN|HOP_FLG_ACK, []byte{}, false)
}

func (srv *ElServer) cleanUp() {
	// Pre Down
	if srv.cfg.Down != "" {
		args := strings.Split(srv.cfg.Down, " ")
		var cmd *exec.Cmd
		if len(args) == 1 {
			cmd = exec.Command(args[0])
		} else {
			cmd = exec.Command(args[0], args[1:]...)
		}
		logger.Info(srv.cfg.Down)
		cmd.Run()
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	for _, hpeer := range srv.peers {
		srv.toClient(hpeer, HOP_FLG_FIN|HOP_FLG_ACK, []byte{}, false)
		srv.toClient(hpeer, HOP_FLG_FIN|HOP_FLG_ACK, []byte{}, false)
		srv.toClient(hpeer, HOP_FLG_FIN, []byte{}, false)
		srv.toClient(hpeer, HOP_FLG_FIN, []byte{}, false)
	}
	clearMSS(srv.iface.Name(), true)
	os.Exit(0)
}

func (srv *ElServer) peerTimeoutWatcher() {
	timeout := time.Second * time.Duration(srv.cfg.PeerTimeout)
	interval := time.Second * time.Duration(srv.cfg.PeerTimeout/2)

	for {
		if srv.cfg.PeerTimeout <= 0 {
			return
		}
		time.Sleep(interval)
		for sid, hpeer := range srv.peers {
			// Heartbeat
			if sid < 0x01<<32 {
				continue
			}
			logger.Debug("IP: %v, sid: %v", hpeer.ip, sid)
			srv.toClient(hpeer, HOP_FLG_PSH, []byte{}, false)
		}
		// count := 0
		time.Sleep(interval)
		for sid, hpeer := range srv.peers {
			if sid < 0x01<<32 {
				continue
			}
			logger.Debug("watch: %v", hpeer.lastSeenTime)
			// if sid>>32 > 0 {
			// 	count++
			// }
			conntime := time.Since(hpeer.lastSeenTime)
			// logger.Debug("watch:%v %v", conntime.Seconds(), timeout.Seconds())
			if conntime > timeout {
				logger.Info("peer %v timeout", hpeer.ip)
				go srv.kickOutPeer(sid)
			}
		}
		// logger.Info("Ulinks:%d", count)
	}
}
