package el

// Handle ElPacket's fragmentation

import (
	"sync"
	"time"
)

const (
	// Fragment Threshold
	FRG_THRES = 32
	// Max Fragments
	MAX_FRAGS = 8
)

// var elFrager *ElFragmenter

type elSequencer interface {
	Seq() uint32
}

type elFragCacheRecord struct {
	ts int64
	p  *ElPacket
}

type elFragCache struct {
	cache       map[uint32]*elFragCacheRecord
	flushPeriod time.Time
	lock        sync.RWMutex
}

func newElFragCache(fp time.Duration) *elFragCache {
	c := new(elFragCache)
	c.cache = make(map[uint32]*elFragCacheRecord)
	go func() {
		ticker := time.NewTicker(fp)
		for {
			<-ticker.C
			c.checkExpired()
		}
	}()
	return c
}

func (c *elFragCache) checkExpired() {
	removeKey := func(k uint32) {
		c.lock.Lock()
		defer c.lock.Unlock()
		delete(c.cache, k)
	}
	nowts := time.Now().Unix()
	for k, v := range c.cache {
		if nowts-v.ts > 60 {
			removeKey(k)
		}
	}
}

func (c *elFragCache) insert(k uint32, p *elFragCacheRecord) {
	c.lock.Lock()
	c.cache[k] = p
	c.lock.Unlock()
}

func (c *elFragCache) get(k uint32) (*elFragCacheRecord, bool) {
	c.lock.RLock()
	v, found := c.cache[k]
	c.lock.RUnlock()
	return v, found
}

type ElFragmenter struct {
	morpher ElMorpher
	cache   *elFragCache
}

func newElFragmenter(m ElMorpher) *ElFragmenter {
	hf := new(ElFragmenter)
	hf.morpher = m
	hf.cache = newElFragCache(30 * time.Second)
	return hf
}

func (hf *ElFragmenter) Fragmentate(c elSequencer, frame []byte) []*ElPacket {
	seq := c.Seq()
	frameSize := len(frame)
	packets := make([]*ElPacket, 0, MAX_FRAGS)

	// Debug start
	/*
	   hp := new(ElPacket)
	   hp.Seq = seq
	   hp.Flag = HOP_FLG_DAT
	   hp.Frag = uint8(0)
	   hp.Plen = uint16(frameSize)
	   hp.FragPrefix = uint16(0)
	   hp.setPayload(frame)
	   logger.Debug("seq: %d, plen: %d, dlen: %d", seq, hp.Plen, hp.Dlen)
	   packets = append(packets, hp)
	   return packets
	*/
	// Debug end

	prefixes := make([]int, 0, MAX_FRAGS)
	prefix := 0
	padding := 0

	for i, restSize := 0, frameSize; i < MAX_FRAGS; i++ {
		fragSize := hf.morpher.NextPackSize()
		//logger.Debug("restSize: %d, fragSize: %d", restSize, fragSize)

		delta := restSize - fragSize

		if delta < FRG_THRES {
			if delta < -FRG_THRES {
				padding = -delta
			}
			prefix += restSize
			prefixes = append(prefixes, prefix)
			break
		} else {
			if i == MAX_FRAGS-1 {
				prefix += restSize
			} else {
				prefix += fragSize
				restSize -= fragSize
			}
		}

		prefixes = append(prefixes, prefix)
	}

	start := 0
	for i, q := range prefixes {
		hp := new(ElPacket)
		hp.Seq = seq
		hp.Flag = HOP_FLG_DAT | HOP_FLG_MFR
		hp.Frag = uint8(i)
		hp.Plen = uint16(frameSize)
		hp.FragPrefix = uint16(start)
		hp.setPayload(frame[start:q])
		packets = append(packets, hp)
		start = q
	}

	//logger.Debug("packSize: %d, fragSize: %v", frameSize, prefixes)
	last := len(packets) - 1
	packets[last].Flag ^= HOP_FLG_MFR
	if padding > 0 {
		packets[last].addNoise(padding)
	}

	return packets

}

func (hf *ElFragmenter) reAssemble(packets []*ElPacket) []*ElPacket {
	rpacks := make([]*ElPacket, 0, len(packets))
	now := time.Now().Unix()

	hf.cache.lock.Lock()
	defer func() {
		hf.cache.lock.Unlock()
		if err := recover(); err != nil {
			logger.Error("Error reassemble packet fragments: %s", err)
		}
	}()

	for _, p := range packets {
		// logger.Debug("frag: %v", p.elPacketHeader)
		if p.Dlen == p.Plen {
			// logger.Debug("rpacket: %v", p.elPacketHeader)
			rpacks = append(rpacks, p)
			continue
		}

		if r, found := hf.cache.cache[p.Seq]; found {
			rp := r.p
			// logger.Debug("plen: %d, recved: %d", rp.Plen, rp.Dlen)
			rp.Dlen += p.Dlen
			s := p.FragPrefix
			e := s + p.Dlen
			rp.Flag ^= ((p.Flag & HOP_FLG_MFR) ^ HOP_FLG_MFR)
			copy(rp.payload[s:e], p.payload)
			if rp.Dlen == rp.Plen {
				rp.Flag ^= HOP_FLG_MFR
				// logger.Debug("rpacket: %v", rp.elPacketHeader)
				rpacks = append(rpacks, rp)
				delete(hf.cache.cache, p.Seq)
			}

		} else {
			payload := make([]byte, p.Plen)
			s := p.FragPrefix
			e := s + p.Dlen
			p.Flag = HOP_FLG_DAT ^ ((p.Flag & HOP_FLG_MFR) ^ HOP_FLG_MFR)
			p.Frag = uint8(0xFF)
			copy(payload[s:e], p.payload)
			p.payload = payload
			record := &elFragCacheRecord{ts: now, p: p}
			hf.cache.cache[p.Seq] = record
		}
	}

	return rpacks

}
