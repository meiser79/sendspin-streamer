// ABOUTME: MP3 encoder stage backed by LAME (cgo), one instance per listener.
package main

import (
	"bytes"

	lame "github.com/viert/go-lame"
)

type mp3Encoder struct {
	buf *bytes.Buffer
	enc *lame.Encoder
}

func newMP3Encoder(o encOpts) (Encoder, error) {
	buf := new(bytes.Buffer)
	enc := lame.NewEncoder(buf)
	if enc == nil {
		return nil, errUnsupported("mp3", "LAME encoder could not be created")
	}
	_ = enc.SetNumChannels(o.Channels)
	_ = enc.SetInSamplerate(o.SampleRate)
	_ = enc.SetBrate(o.Bitrate)
	_ = enc.SetMode(lame.MpegJointStereo)
	_ = enc.SetQuality(3)
	enc.SetWriteID3TagAutomatic(false)
	return &mp3Encoder{buf: buf, enc: enc}, nil
}

func (e *mp3Encoder) Header() []byte { return e.take() }

func (e *mp3Encoder) Encode(pcm []int16) ([]byte, error) {
	raw := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		u := uint16(v)
		raw[i*2] = byte(u)
		raw[i*2+1] = byte(u >> 8)
	}
	if _, err := e.enc.Write(raw); err != nil {
		return nil, err
	}
	return e.take(), nil
}

func (e *mp3Encoder) Close() []byte {
	if e.enc == nil {
		return nil
	}
	_, _ = e.enc.Flush()
	e.enc.Close()
	e.enc = nil
	return e.take()
}

func (e *mp3Encoder) take() []byte {
	if e.buf.Len() == 0 {
		return nil
	}
	out := make([]byte, e.buf.Len())
	copy(out, e.buf.Bytes())
	e.buf.Reset()
	return out
}
