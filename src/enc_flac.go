// ABOUTME: Native FLAC encoder stage (pure Go, lossless) with 4096-sample blocks.
package main

import (
	"bytes"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

const flacBlock = 4096

type flacEncoder struct {
	buf    *bytes.Buffer
	enc    *flac.Encoder
	o      encOpts
	pend   []int16 // interleaved leftovers below one block
	header []byte
}

func newFLACEncoder(o encOpts) (Encoder, error) {
	buf := new(bytes.Buffer)
	info := &meta.StreamInfo{
		BlockSizeMin:  flacBlock,
		BlockSizeMax:  flacBlock,
		SampleRate:    uint32(o.SampleRate),
		NChannels:     uint8(o.Channels),
		BitsPerSample: 16,
		NSamples:      0, // unknown: endless stream
	}
	enc, err := flac.NewEncoder(buf, info)
	if err != nil {
		return nil, err
	}
	e := &flacEncoder{buf: buf, enc: enc, o: o}
	e.header = e.take()
	return e, nil
}

func (e *flacEncoder) Header() []byte { return e.header }

func (e *flacEncoder) Encode(pcm []int16) ([]byte, error) {
	e.pend = append(e.pend, pcm...)
	ch := e.o.Channels
	for len(e.pend) >= flacBlock*ch {
		block := e.pend[:flacBlock*ch]
		if err := e.writeBlock(block); err != nil {
			return nil, err
		}
		e.pend = e.pend[flacBlock*ch:]
	}
	return e.take(), nil
}

func (e *flacEncoder) writeBlock(block []int16) error {
	ch := e.o.Channels
	n := len(block) / ch
	subs := make([]*frame.Subframe, ch)
	for c := 0; c < ch; c++ {
		s := make([]int32, n)
		for i := 0; i < n; i++ {
			s[i] = int32(block[i*ch+c])
		}
		subs[c] = &frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   s,
			NSamples:  n,
		}
	}
	channels := frame.ChannelsLR
	if ch == 1 {
		channels = frame.ChannelsMono
	}
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: false,
			BlockSize:         uint16(n),
			SampleRate:        uint32(e.o.SampleRate),
			Channels:          channels,
			BitsPerSample:     16,
		},
		Subframes: subs,
	}
	return e.enc.WriteFrame(f)
}

func (e *flacEncoder) Close() []byte {
	e.enc = nil
	e.pend = nil
	return nil
}

func (e *flacEncoder) take() []byte {
	if e.buf.Len() == 0 {
		return nil
	}
	out := make([]byte, e.buf.Len())
	copy(out, e.buf.Bytes())
	e.buf.Reset()
	return out
}
