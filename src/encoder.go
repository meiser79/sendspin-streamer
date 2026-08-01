// ABOUTME: Encoder is the modular output stage: every HTTP listener gets its
// ABOUTME: own encoder instance for the requested format (pcm/wav/mp3/flac/opus).
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// Encoder turns paced interleaved stereo s16 PCM into a container/codec
// bitstream. One instance per listener; never used concurrently.
type Encoder interface {
	// Header returns the bytes to send before the first audio data (WAV/FLAC
	// headers, Ogg header pages); may be empty.
	Header() []byte
	// Encode consumes one PCM slice and returns whatever bytes are ready.
	Encode(pcm []int16) ([]byte, error)
	// Close releases native resources and returns trailing bytes, if any.
	Close() []byte
}

// Format describes one streaming endpoint.
type Format struct {
	Name        string // "mp3"
	Path        string // "/stream.mp3"
	ContentType string
	ICY         bool // ICY in-stream metadata is meaningful for this format
	Desc        string
	New         func(o encOpts) (Encoder, error)
}

// encOpts carries everything an encoder needs to start.
type encOpts struct {
	SampleRate int
	Channels   int
	Bitrate    int // kbps, for lossy codecs
	Quality    int
}

var formats = map[string]*Format{
	"mp3": {
		Name: "mp3", Path: "/stream.mp3", ContentType: "audio/mpeg", ICY: true,
		Desc: "MPEG-1 Layer III (LAME), lossy",
		New:  newMP3Encoder,
	},
	"flac": {
		Name: "flac", Path: "/stream.flac", ContentType: "audio/flac",
		Desc: "native FLAC stream, lossless",
		New:  newFLACEncoder,
	},
	"opus": {
		Name: "opus", Path: "/stream.opus", ContentType: "audio/ogg; codecs=opus",
		Desc: "Opus in Ogg, lossy (needs 8/12/16/24/48 kHz source)",
		New:  newOpusEncoder,
	},
	"wav": {
		Name: "wav", Path: "/stream.wav", ContentType: "audio/wav",
		Desc: "PCM s16le with a streaming WAV header, lossless",
		New:  func(o encOpts) (Encoder, error) { return &pcmEncoder{o: o, wav: true}, nil },
	},
	"pcm": {
		Name: "pcm", Path: "/stream.pcm", ContentType: "audio/L16",
		Desc: "raw PCM s16le, no container",
		New:  func(o encOpts) (Encoder, error) { return &pcmEncoder{o: o}, nil },
	},
}

// formatNames returns all known format keys in a stable order.
func formatNames() []string {
	out := make([]string, 0, len(formats))
	for k := range formats {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// canonicalFormat maps a name, extension or alias to a known format key.
func canonicalFormat(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, ".")
	s = strings.TrimPrefix(s, "/stream.")
	switch s {
	case "mpeg", "mp3", "mpga":
		s = "mp3"
	case "ogg", "opus":
		s = "opus"
	case "wave", "wav":
		s = "wav"
	case "raw", "s16le", "l16", "pcm":
		s = "pcm"
	}
	_, ok := formats[s]
	return s, ok
}

// parseFormats turns the STREAM_FORMATS / DEFAULT_FORMAT settings into the
// list of enabled formats and the format served on "/stream". "all" (or an
// empty value) enables every format; unknown entries are ignored. If nothing
// valid remains, everything is enabled so the streamer is never mute.
func parseFormats(list, def string) ([]string, string) {
	enabled := []string{}
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\t'
	}) {
		if strings.EqualFold(part, "all") || strings.EqualFold(part, "*") {
			enabled = formatNames()
			seen = map[string]bool{}
			for _, n := range enabled {
				seen[n] = true
			}
			continue
		}
		if n, ok := canonicalFormat(part); ok && !seen[n] {
			seen[n] = true
			enabled = append(enabled, n)
		}
	}
	if len(enabled) == 0 {
		enabled = formatNames()
		seen = map[string]bool{}
		for _, n := range enabled {
			seen[n] = true
		}
	}
	d, ok := canonicalFormat(def)
	if !ok || !seen[d] {
		d = enabled[0]
		if seen["mp3"] {
			d = "mp3"
		}
	}
	return enabled, d
}

// lookupFormat resolves a format name or a request path to an enabled Format.
func lookupFormat(s string, enabled []string, def string) (*Format, bool) {
	n, ok := canonicalFormat(s)
	if strings.TrimSpace(s) == "" || strings.EqualFold(strings.TrimSpace(s), "default") {
		n, ok = def, true
	}
	if !ok {
		return nil, false
	}
	for _, e := range enabled {
		if e == n {
			return formats[n], true
		}
	}
	return nil, false
}


/* ------------------------------------------------------------- PCM / WAV */

// pcmEncoder serialises the samples as raw little-endian s16. WAV mode
// additionally writes a streaming WAV header with "unknown" (max) sizes, so
// players can decode /stream.wav without being told the format.
type pcmEncoder struct {
	o   encOpts
	wav bool
}

func (e *pcmEncoder) Header() []byte {
	if !e.wav {
		return nil
	}
	ch, sr := e.o.Channels, e.o.SampleRate
	byteRate := sr * ch * 2
	h := new(bytes.Buffer)
	h.WriteString("RIFF")
	binary.Write(h, binary.LittleEndian, uint32(0xFFFFFFFF)) // unknown length
	h.WriteString("WAVEfmt ")
	binary.Write(h, binary.LittleEndian, uint32(16))
	binary.Write(h, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(h, binary.LittleEndian, uint16(ch))
	binary.Write(h, binary.LittleEndian, uint32(sr))
	binary.Write(h, binary.LittleEndian, uint32(byteRate))
	binary.Write(h, binary.LittleEndian, uint16(ch*2)) // block align
	binary.Write(h, binary.LittleEndian, uint16(16))   // bits per sample
	h.WriteString("data")
	binary.Write(h, binary.LittleEndian, uint32(0xFFFFFFFF))
	return h.Bytes()
}

func (e *pcmEncoder) Encode(pcm []int16) ([]byte, error) {
	buf := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		u := uint16(v)
		buf[i*2] = byte(u)
		buf[i*2+1] = byte(u >> 8)
	}
	return buf, nil
}


func (e *pcmEncoder) Close() []byte { return nil }

/* -------------------------------------------------------------- helpers */

// contentType returns the media type for a format. Raw PCM is served as an
// opaque byte stream on purpose: audio/L16 implies big-endian samples and
// several players (ffmpeg among them) drop the rate/channels parameters, so
// they would misinterpret the stream. Clients read the X-Audio-* headers or
// use /stream.wav, which is self-describing.
func contentType(f *Format, rate, channels int) string {
	_ = rate
	_ = channels
	return f.ContentType
}



func errUnsupported(format string, reason string) error {
	return fmt.Errorf("%s: %s", format, reason)
}
