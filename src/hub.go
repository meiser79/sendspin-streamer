// ABOUTME: Hub implements the Sendspin output.Output interface and paces the
// ABOUTME: decoded PCM out to all listeners; encoding happens per connection.
package main

import (
	"log"
	"math"
	"sync"
	"time"
)

const tickMs = 20

// pcmChunk is one paced slice of interleaved stereo s16 audio. Rate/FormatVer
// let a listener notice that the source format changed underneath it.
type pcmChunk struct {
	Samples    []int16
	SampleRate int
	FormatVer  uint64
}

// Hub is the audio sink of the Sendspin player and the PCM source for every
// HTTP listener. It satisfies sendspin-go's audio/output.Output interface.
type Hub struct {
	bufferMs int

	mu         sync.Mutex
	ring       []int16 // interleaved stereo s16
	primed     bool
	sampleRate int
	channels   int
	formatVer  uint64
	volume     int
	muted      bool
	title      string
	titleVer   uint64
	lastAudio  time.Time // last time the source actually delivered samples

	lmu       sync.Mutex
	listeners map[chan pcmChunk]*listener

	stop chan struct{}
	once sync.Once
}

func newHub(bufferMs int) *Hub {
	return &Hub{
		bufferMs:   bufferMs,
		sampleRate: 48000,
		channels:   2,
		volume:     100,
		listeners:  make(map[chan pcmChunk]*listener),
		stop:       make(chan struct{}),
	}
}

/* ------------------------------------------------------------- broadcast */

// listener tracks one HTTP consumer and how long it has been unable to keep
// up. A consumer whose TCP socket stalls (a paused/crashed player, a dead
// network path) would otherwise sit in the map forever, holding its buffered
// chunks and its goroutine, so it is evicted after maxStalledTicks.
type listener struct {
	drops int
}

// maxStalledTicks is how many consecutive paced slices a listener may miss
// before it is dropped (tickMs each, so ~5 seconds).
const maxStalledTicks = 5000 / tickMs

func (h *Hub) broadcast(c pcmChunk) {
	var evicted int
	h.lmu.Lock()
	for ch, l := range h.listeners {
		select {
		case ch <- c:
			l.drops = 0
		default: // slow listener: drop the slice instead of stalling everybody
			l.drops++
			if l.drops >= maxStalledTicks {
				// Stalled for good: free the slot so the writer goroutine
				// returns instead of piling up chunks forever.
				delete(h.listeners, ch)
				close(ch)
				evicted++
			}
		}
	}
	n := len(h.listeners)
	h.lmu.Unlock()
	if evicted > 0 {
		log.Printf("dropped %d stalled listener(s), %d left", evicted, n)
		st.with(func(s *stats) { s.Listeners = n })
	}
}

func (h *Hub) AddListener() chan pcmChunk {
	ch := make(chan pcmChunk, 64)
	h.lmu.Lock()
	h.listeners[ch] = &listener{}
	n := len(h.listeners)
	h.lmu.Unlock()
	st.with(func(s *stats) { s.Listeners = n })
	return ch
}

func (h *Hub) RemoveListener(ch chan pcmChunk) {
	h.lmu.Lock()
	if _, ok := h.listeners[ch]; ok {
		delete(h.listeners, ch)
		close(ch)
	}
	n := len(h.listeners)
	h.lmu.Unlock()
	st.with(func(s *stats) { s.Listeners = n })
}

func (h *Hub) Listeners() int {
	h.lmu.Lock()
	defer h.lmu.Unlock()
	return len(h.listeners)
}

/* ---------------------------------------------------------- output.Output */

// Open is called by the player when a stream starts (or the format changes).
func (h *Hub) Open(sampleRate, channels, bitDepth int) error {
	h.mu.Lock()
	if sampleRate != h.sampleRate {
		h.formatVer++
	}
	h.sampleRate = sampleRate
	h.channels = channels
	h.ring = nil
	h.primed = false
	h.mu.Unlock()

	st.with(func(s *stats) {
		s.SampleRte = sampleRate
		s.Channels = channels
	})
	log.Printf("stream: %d Hz, %d ch, %d bit", sampleRate, channels, bitDepth)
	return nil
}

// Write takes decoded samples (int32 in 24-bit range, interleaved).
func (h *Hub) Write(samples []int32) error {
	if len(samples) == 0 {
		return nil
	}
	src := h.channels
	if src < 1 {
		src = 2
	}
	frames := len(samples) / src
	out := make([]int16, 0, frames*2)
	for i := 0; i < frames; i++ {
		l := samples[i*src] >> 8
		var r int32
		if src > 1 {
			r = samples[i*src+1] >> 8
		} else {
			r = l
		}
		out = append(out, clip16(l), clip16(r))
	}

	h.mu.Lock()
	h.lastAudio = time.Now()
	h.ring = append(h.ring, out...)
	// never buffer more than pre-buffer + 4s
	max := (h.bufferMs + 4000) * h.sampleRate / 1000 * 2
	if len(h.ring) > max {
		h.ring = h.ring[len(h.ring)-max:]
	}
	if !h.primed && len(h.ring) >= h.bufferMs*h.sampleRate/1000*2 {
		h.primed = true
	}
	h.mu.Unlock()

	st.with(func(s *stats) { s.Frames += int64(frames) })
	return nil
}

func (h *Hub) Close() error {
	st.with(func(s *stats) { s.Streaming = false })
	h.mu.Lock()
	h.lastAudio = time.Time{}
	h.ring = nil
	h.primed = false
	h.mu.Unlock()
	return nil
}

func (h *Hub) SetVolume(v int) {
	h.mu.Lock()
	h.volume = v
	h.mu.Unlock()
	st.with(func(s *stats) { s.Volume = v })
}

func (h *Hub) SetMuted(m bool) {
	h.mu.Lock()
	h.muted = m
	h.mu.Unlock()
	st.with(func(s *stats) { s.Muted = m })
}

// SetTitle stores the current track for ICY in-stream metadata.
func (h *Hub) SetTitle(t string) {
	h.mu.Lock()
	if t != h.title {
		h.title = t
		h.titleVer++
	}
	h.mu.Unlock()
}

// Title returns the current track plus a version counter that changes whenever
// the title changes, so listeners know when to inject a new metadata block.
func (h *Hub) Title() (string, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.title, h.titleVer
}

// Format returns the current PCM format of the hub (stereo s16 is implied).
func (h *Hub) Format() (sampleRate int, ver uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sampleRate, h.formatVer
}

func (h *Hub) BufferedMs() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sampleRate == 0 {
		return 0
	}
	return len(h.ring) / 2 * 1000 / h.sampleRate
}

/* ------------------------------------------------------------ pacing loop */

// Start runs the master clock: every tick a fixed slice of PCM is handed to
// the listeners — silence when the jitter buffer is empty — so no stream ends.
func (h *Hub) Start() {
	go func() {
		ticker := time.NewTicker(tickMs * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				h.tick()
			}
		}
	}()
}

func (h *Hub) Stop() {
	h.once.Do(func() {
		close(h.stop)
		h.lmu.Lock()
		for ch := range h.listeners {
			delete(h.listeners, ch)
			close(ch)
		}
		h.lmu.Unlock()
	})
}

func (h *Hub) tick() {
	h.mu.Lock()
	rate := h.sampleRate
	ver := h.formatVer
	need := rate * tickMs / 1000 * 2
	gain := 0.0
	if !h.muted {
		gain = math.Pow(float64(h.volume)/100.0, 1.5)
	}
	// the source counts as active only while it keeps delivering samples
	live := !h.lastAudio.IsZero() && time.Since(h.lastAudio) < time.Second
	pcm := make([]int16, need)
	if h.primed && len(h.ring) > 0 {
		n := need
		if len(h.ring) < n {
			n = len(h.ring)
			st.with(func(s *stats) { s.Underruns++ })
		}
		copy(pcm, h.ring[:n])
		h.ring = h.ring[n:]
		if len(h.ring) == 0 {
			h.primed = false
		}
	}
	h.mu.Unlock()

	st.with(func(s *stats) { s.Streaming = live })

	if gain != 1.0 {
		for i, v := range pcm {
			pcm[i] = clip16(int32(float64(v) * gain))
		}
	}
	h.broadcast(pcmChunk{Samples: pcm, SampleRate: rate, FormatVer: ver})
}

func clip16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
