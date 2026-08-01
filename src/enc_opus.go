// ABOUTME: Opus encoder stage (libopus via cgo) muxed into an Ogg stream with
// ABOUTME: 20 ms packets, OpusHead/OpusTags header pages and 48 kHz granules.
package main

import (
	"encoding/binary"
	"math/rand"

	opus "gopkg.in/hraban/opus.v2"
)

const opusPreSkip = 312 // 6.5 ms at 48 kHz, libopus default lookahead

type opusEncoder struct {
	o       encOpts
	enc     *opus.Encoder
	ogg     *oggWriter
	frame   int // samples per channel per packet (20 ms)
	pend    []int16
	granule uint64
	header  []byte
	pkt     []byte
}

func newOpusEncoder(o encOpts) (Encoder, error) {
	switch o.SampleRate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return nil, errUnsupported("opus",
			"source sample rate must be 8/12/16/24/48 kHz")
	}
	enc, err := opus.NewEncoder(o.SampleRate, o.Channels, opus.AppAudio)
	if err != nil {
		return nil, err
	}
	if o.Bitrate > 0 {
		_ = enc.SetBitrate(o.Bitrate * 1000)
	}
	_ = enc.SetComplexity(8)

	e := &opusEncoder{
		o:     o,
		enc:   enc,
		ogg:   &oggWriter{serial: rand.Uint32() | 1},
		frame: o.SampleRate / 50, // 20 ms
		pkt:   make([]byte, 4000),
	}
	e.header = append(e.ogg.page(e.opusHead(), 0x02, 0), e.ogg.page(opusTags(), 0x00, 0)...)
	return e, nil
}

func (e *opusEncoder) opusHead() []byte {
	h := make([]byte, 19)
	copy(h, "OpusHead")
	h[8] = 1                  // version
	h[9] = byte(e.o.Channels) // channel count
	binary.LittleEndian.PutUint16(h[10:], opusPreSkip)
	binary.LittleEndian.PutUint32(h[12:], uint32(e.o.SampleRate))
	binary.LittleEndian.PutUint16(h[16:], 0) // output gain
	h[18] = 0                                // channel mapping family
	return h
}

func opusTags() []byte {
	vendor := "sendspin-streamer"
	t := make([]byte, 0, 8+4+len(vendor)+4)
	t = append(t, []byte("OpusTags")...)
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], uint32(len(vendor)))
	t = append(t, b4[:]...)
	t = append(t, vendor...)
	binary.LittleEndian.PutUint32(b4[:], 0) // no user comments
	t = append(t, b4[:]...)
	return t
}

func (e *opusEncoder) Header() []byte { return e.header }

func (e *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	e.pend = append(e.pend, pcm...)
	need := e.frame * e.o.Channels
	// granule positions are always counted at 48 kHz
	step := uint64(e.frame * 48000 / e.o.SampleRate)

	var out []byte
	for len(e.pend) >= need {
		n, err := e.enc.Encode(e.pend[:need], e.pkt)
		if err != nil {
			return nil, err
		}
		e.pend = e.pend[need:]
		e.granule += step
		out = append(out, e.ogg.page(e.pkt[:n], 0x00, e.granule)...)
	}
	return out, nil
}

func (e *opusEncoder) Close() []byte {
	e.enc = nil
	e.pend = nil
	// final empty page marks the end of stream for well-behaved clients
	return e.ogg.page(nil, 0x04, e.granule)
}
