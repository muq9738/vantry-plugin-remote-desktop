package rtc

import (
	"errors"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// accessUnits emits a complete contiguous marker-terminated H264 frame without
// waiting for the next frame's timestamp. A sample-builder's extra frame delay
// is material for a static desktop. Reordering is retained only for a short,
// bounded interval; a missing reference chain asks for IDR instead of backing up.
type accessUnits struct {
	groups       map[uint32]*accessUnit
	depacketizer codecs.H264Packet
	last         uint32
	delivered    bool
	captureID    uint8
	dropped      uint64
}
type accessUnit struct {
	packets   map[uint16]*rtp.Packet
	first     uint16
	tail      uint16
	haveFirst bool
	haveTail  bool
	created   time.Time
	capture   time.Time
}

func newAccessUnits(captureID uint8) *accessUnits {
	return &accessUnits{groups: map[uint32]*accessUnit{}, captureID: captureID}
}

// expire runs on both incoming packets and receive timeouts, including a
// static screen that produces no later RTP packet after a lost marker.
func (a *accessUnits) expire(now time.Time) bool {
	needKey := false
	for ts, g := range a.groups {
		if now.Sub(g.created) > 60*time.Millisecond {
			delete(a.groups, ts)
			a.dropped++
			needKey = true
		}
	}
	return needKey
}
func (a *accessUnits) push(packet *rtp.Packet, now time.Time) (*EncodedFrame, bool, error) {
	if a.delivered && int32(packet.Timestamp-a.last) <= 0 {
		return nil, false, nil
	}
	needKey := a.expire(now)
	g := a.groups[packet.Timestamp]
	if g == nil {
		if len(a.groups) >= 3 {
			var oldest uint32
			var at time.Time
			for ts, group := range a.groups {
				if at.IsZero() || group.created.Before(at) {
					oldest, at = ts, group.created
				}
			}
			delete(a.groups, oldest)
			a.dropped++
			needKey = true
		}
		g = &accessUnit{packets: map[uint16]*rtp.Packet{}, created: now}
		a.groups[packet.Timestamp] = g
	}
	if len(g.packets) >= 4096 {
		return nil, true, errors.New("rtc: H264 frame exceeds bounded reorder window")
	}
	g.packets[packet.SequenceNumber] = packet
	if a.depacketizer.IsPartitionHead(packet.Payload) && (!g.haveFirst || int16(packet.SequenceNumber-g.first) < 0) {
		g.first, g.haveFirst = packet.SequenceNumber, true
	}
	if packet.Marker {
		g.tail, g.haveTail = packet.SequenceNumber, true
	}
	if a.captureID != 0 {
		if raw := packet.GetExtension(a.captureID); len(raw) > 0 {
			var ext rtp.AbsCaptureTimeExtension
			if ext.Unmarshal(raw) == nil {
				g.capture = ext.CaptureTime()
			}
		}
	}
	if !g.haveFirst || !g.haveTail {
		return nil, needKey, nil
	}
	count := int(uint16(g.tail-g.first)) + 1
	if count > 4096 {
		return nil, true, errors.New("rtc: invalid H264 sequence span")
	}
	for n := 0; n < count; n++ {
		if g.packets[g.first+uint16(n)] == nil {
			return nil, needKey, nil
		}
	}
	var data []byte
	for n := 0; n < count; n++ {
		payload, err := a.depacketizer.Unmarshal(g.packets[g.first+uint16(n)].Payload)
		if err != nil {
			delete(a.groups, packet.Timestamp)
			return nil, true, nil
		}
		data = append(data, payload...)
	}
	delete(a.groups, packet.Timestamp)
	a.last, a.delivered = packet.Timestamp, true
	for ts := range a.groups {
		if int32(ts-a.last) < 0 {
			delete(a.groups, ts)
			a.dropped++
			needKey = true
		}
	}
	return &EncodedFrame{CapturedAt: g.capture, KeyFrame: containsNAL(data, 5), Data: data}, needKey, nil
}
func containsNAL(b []byte, kind byte) bool {
	for i := 0; i+3 < len(b); i++ {
		n := 0
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			n = i + 3
		} else if i+4 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			n = i + 4
		}
		if n != 0 && b[n]&31 == kind {
			return true
		}
	}
	return false
}
