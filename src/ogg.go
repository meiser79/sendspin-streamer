// ABOUTME: Minimal Ogg container muxer: builds pages with lacing table and CRC
// ABOUTME: so Opus packets can be streamed without an external dependency.
package main

import "encoding/binary"

var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}

type oggWriter struct {
	serial uint32
	seq    uint32
}

// page wraps one packet into a single Ogg page. Packets stay well below the
// 255*255 byte limit for Opus, so no continuation pages are needed.
func (o *oggWriter) page(packet []byte, headerType byte, granule uint64) []byte {
	segs := len(packet)/255 + 1
	if segs > 255 {
		segs = 255
	}
	out := make([]byte, 0, 27+segs+len(packet))
	out = append(out, 'O', 'g', 'g', 'S')
	out = append(out, 0)          // version
	out = append(out, headerType) // 0x02 BOS, 0x04 EOS
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], granule)
	out = append(out, b8[:]...)
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], o.serial)
	out = append(out, b4[:]...)
	binary.LittleEndian.PutUint32(b4[:], o.seq)
	out = append(out, b4[:]...)
	o.seq++
	out = append(out, 0, 0, 0, 0) // CRC placeholder
	out = append(out, byte(segs))
	rest := len(packet)
	for i := 0; i < segs; i++ {
		if rest >= 255 {
			out = append(out, 255)
			rest -= 255
		} else {
			out = append(out, byte(rest))
			rest = 0
		}
	}
	out = append(out, packet...)
	crc := oggCRC(out)
	binary.LittleEndian.PutUint32(out[22:26], crc)
	return out
}
