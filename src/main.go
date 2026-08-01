// ABOUTME: sendspin-streamer — Sendspin player@v1 client that re-broadcasts
// ABOUTME: the received audio as a continuous HTTP stream. One binary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/discovery"
	"github.com/Sendspin/sendspin-go/pkg/sendspin"
)

/* ----------------------------------------------------------------- config */

type config struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Genre          string   `json:"genre"`
	ClientID       string   `json:"client_id"`
	ServerAddr     string   `json:"server_addr"`
	HTTPPort       int      `json:"http_port"`
	Bitrate        int      `json:"mp3_bitrate"`
	BufferMs       int      `json:"buffer_ms"`
	Volume         int      `json:"volume"`
	MetaInt        int      `json:"icy_metaint"`
	PreferredCodec string   `json:"preferred_codec"`
	MaxSampleRate  int      `json:"max_sample_rate"`
	MaxBitDepth    int      `json:"max_bit_depth"`
	Mdns           bool     `json:"mdns_discovery"`
	Formats        []string `json:"formats"`
	DefaultFormat  string   `json:"default_format"`
}



func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func loadConfig() config {
	host, _ := os.Hostname()
	if host == "" {
		host = "sendspin"
	}
	enabled, def := parseFormats(
		env("STREAM_FORMATS", env("STREAM_FORMAT", env("FORMATS", "all"))),
		env("DEFAULT_FORMAT", ""),
	)
	return config{
		Name:           env("NAME", "Sendspin Streamer ("+host+")"),
		Description:    env("ICY_DESCRIPTION", "Sendspin Streamer"),
		Genre:          env("ICY_GENRE", "Various"),
		ClientID:       env("CLIENT_ID", ""),
		ServerAddr:     env("SENDSPIN_SERVER", ""),
		HTTPPort:       envInt("HTTP_PORT", 8000),
		Bitrate:        envInt("MP3_BITRATE", 192),
		BufferMs:       envInt("BUFFER_MS", 800),
		Volume:         envInt("VOLUME", 100),
		MetaInt:        envInt("ICY_METAINT", 16000),
		PreferredCodec: env("PREFERRED_CODEC", "pcm"),
		MaxSampleRate:  envInt("MAX_SAMPLE_RATE", 48000),
		MaxBitDepth:    envInt("MAX_BIT_DEPTH", 16),
		Mdns:           env("MDNS", "1") != "0",
		Formats:        enabled,
		DefaultFormat:  def,
	}
}


// icyMetaBlock builds a Shoutcast metadata block: one length byte counting
// 16-byte units, followed by the padded "StreamTitle='…';" payload.
func icyMetaBlock(title, fallback string) []byte {
	t := strings.TrimSpace(title)
	if t == "" {
		t = fallback
	}
	t = strings.NewReplacer("'", "`", ";", ",", "\x00", "").Replace(t)
	payload := "StreamTitle='" + t + "';StreamUrl='';"
	if len(payload) > 4080 { // 255 * 16
		payload = payload[:4080]
	}
	blocks := (len(payload) + 15) / 16
	out := make([]byte, 1+blocks*16)
	out[0] = byte(blocks)
	copy(out[1:], payload)
	return out
}


/* ------------------------------------------------------------------ stats */

type stats struct {
	mu        sync.Mutex
	Listeners int    `json:"listeners"`
	BytesOut  int64  `json:"bytes_out"`
	Underruns int64  `json:"underruns"`
	Frames    int64  `json:"frames_in"`
	SampleRte int    `json:"stream_sample_rate"`
	Channels  int    `json:"stream_channels"`
	Streaming bool   `json:"streaming"`
	Server    string `json:"server"`
	State     string `json:"state"`
	Volume    int    `json:"volume"`
	Muted     bool   `json:"muted"`
	Now       string `json:"now_playing"`

	StartedAt time.Time
}

var st = &stats{StartedAt: time.Now()}

func (s *stats) with(f func(*stats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *stats) snapshot(cfg config, bufMs int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"name":               cfg.Name,
		"listeners":          s.Listeners,
		"bytes_out":          s.BytesOut,
		"underruns":          s.Underruns,
		"frames_in":          s.Frames,
		"stream_sample_rate": s.SampleRte,
		"stream_channels":    s.Channels,
		"streaming":          s.Streaming,
		"output_active":      s.Listeners > 0,
		"server":             s.Server,
		"state":              s.State,
		"volume":             s.Volume,
		"muted":              s.Muted,
		"now_playing":        s.Now,

		"buffer_ms":          bufMs,
		"uptime_sec":         int(time.Since(s.StartedAt).Seconds()),
		"config":             cfg,
	}
}

/* ------------------------------------------------------------------- main */

func main() {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags)

	clientID, err := sendspin.ResolveClientID(cfg.ClientID, "", nil)
	if err != nil {
		log.Fatalf("Client ID: %v", err)
	}
	cfg.ClientID = clientID

	hub := newHub(cfg.BufferMs)
	hub.SetVolume(cfg.Volume)
	hub.Start()
	defer hub.Stop()

	go serveHTTP(cfg, hub)

	addr := cfg.ServerAddr
	if addr == "" {
		if !cfg.Mdns {
			log.Fatal("no SENDSPIN_SERVER configured and MDNS=0")
		}
		log.Printf("searching Sendspin server via mDNS ...")
		found, err := discover(context.Background(), 0)
		if err != nil {
			log.Fatalf("discovery: %v", err)
		}
		addr = found
	}
	log.Printf("server: %s", addr)

	playerCfg := sendspin.PlayerConfig{
		ServerAddr:     addr,
		PlayerName:     cfg.Name,
		ClientID:       cfg.ClientID,
		Volume:         cfg.Volume,
		BufferMs:       cfg.BufferMs,
		PreferredCodec: cfg.PreferredCodec,
		MaxSampleRate:  cfg.MaxSampleRate,
		MaxBitDepth:    cfg.MaxBitDepth,
		Output:         hub,
		DeviceInfo: sendspin.DeviceInfo{
			ProductName:     "sendspin-streamer",
			Manufacturer:    "sendspin-streamer",
			SoftwareVersion: "2.0.0",
		},
		OnMetadata: func(m sendspin.Metadata) {
			log.Printf("now playing: %s — %s (%s)", m.Artist, m.Title, m.Album)
			title := strings.TrimSpace(m.Title)
			if a := strings.TrimSpace(m.Artist); a != "" && title != "" {
				title = a + " - " + title
			} else if title == "" {
				title = a
			}
			hub.SetTitle(title)
			st.with(func(s *stats) { s.Now = title })
		},

		OnStateChange: func(p sendspin.PlayerState) {
			st.with(func(s *stats) {
				s.State = string(p.State)
				s.Volume = p.Volume
				s.Muted = p.Muted
				s.Server = addr
			})
		},
		OnError: func(err error) { log.Printf("player error: %v", err) },
		Reconnect: sendspin.ReconnectConfig{
			Enabled:      true,
			InitialDelay: 500 * time.Millisecond,
			MaxDelay:     30 * time.Second,
			Multiplier:   2.0,
			Rediscover: func(ctx context.Context) (string, error) {
				if cfg.ServerAddr != "" || !cfg.Mdns {
					return "", nil
				}
				return discover(ctx, 5*time.Second)
			},
		},
	}

	player, err := sendspin.NewPlayer(playerCfg)
	if err != nil {
		log.Fatalf("player: %v", err)
	}
	defer player.Close()

	if err := player.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	if err := player.Play(); err != nil {
		log.Fatalf("play: %v", err)
	}
	log.Printf("connected — streams: %s (default /stream = %s)",
		strings.Join(cfg.Formats, ", "), cfg.DefaultFormat)

	// The Sendspin session can survive a clock-sync stall or a websocket
	// teardown without ever returning to "playing": the library rebuilds the
	// connection but nobody re-announces this client as an active player, so
	// the controller keeps showing it as unreachable. Supervise it.
	go supervise(player, cfg)


	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("shutdown ...")
}

/* ------------------------------------------------------------- supervisor */

// supervise re-arms the player after a reconnect. sendspin-go restores the
// websocket on its own, but it leaves the player state at "reconnecting" and
// never re-sends the client state, so the server stops treating this client as
// a usable playback target. Re-issuing Play() (plus volume/mute) puts it back
// into the group; a failing Play() means the socket is gone again and the
// library's own backoff takes over until the next check.
func supervise(player *sendspin.Player, cfg config) {
	const checkEvery = 5 * time.Second

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()

	var (
		wasConnected = true
		lastRearm    time.Time
	)

	for range ticker.C {
		s := player.Status()

		if !s.Connected {
			if wasConnected {
				log.Printf("session: disconnected, waiting for reconnect ...")
			}
			wasConnected = false
			continue
		}

		if !wasConnected {
			log.Printf("session: reconnected, re-announcing player")
			wasConnected = true
			lastRearm = time.Time{} // force an immediate re-arm below
		}

		// "playing" is the only state in which the server keeps this client in
		// the sync group. Anything else after a reconnect is stale.
		if s.State == "playing" {
			continue
		}
		if time.Since(lastRearm) < 15*time.Second {
			continue
		}
		lastRearm = time.Now()

		log.Printf("session: state %q, re-issuing play", s.State)
		if err := player.Play(); err != nil {
			log.Printf("session: play failed: %v", err)
			continue
		}
		if err := player.SetVolume(s.Volume); err != nil {
			log.Printf("session: volume resync failed: %v", err)
		}
		if err := player.Mute(s.Muted); err != nil {
			log.Printf("session: mute resync failed: %v", err)
		}
	}
	_ = cfg
}

/* -------------------------------------------------------------- discovery */

func discover(ctx context.Context, timeout time.Duration) (string, error) {
	mgr := discovery.NewManager(discovery.Config{ServiceName: "sendspin-streamer"})
	if err := mgr.Browse(); err != nil {
		return "", err
	}
	defer mgr.Stop()

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case srv := <-mgr.Servers():
		if srv == nil {
			return "", fmt.Errorf("no server found")
		}
		log.Printf("mDNS: %q on %s:%d", srv.Name, srv.Host, srv.Port)
		return fmt.Sprintf("%s:%d", srv.Host, srv.Port), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

/* ------------------------------------------------------------------- http */

func serveHTTP(cfg config, hub *Hub) {
	mux := http.NewServeMux()

	// streamHandler builds an HTTP handler for one output format. Every
	// listener gets its own encoder instance fed from the hub's PCM feed.
	streamHandler := func(f *Format) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming not supported", http.StatusInternalServerError)
				return
			}

			rate, formatVer := hub.Format()
			enc, err := f.New(encOpts{
				SampleRate: rate,
				Channels:   2,
				Bitrate:    cfg.Bitrate,
			})
			if err != nil {
				log.Printf("encoder %s: %v", f.Name, err)
				http.Error(w, "format unavailable: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			defer enc.Close()

			// ICY in-stream metadata only for formats that carry it (MP3).
			wantMeta := f.ICY && cfg.MetaInt > 0 &&
				strings.HasPrefix(strings.ToLower(r.Header.Get("Icy-MetaData")), "1")

			h := w.Header()
			h.Set("Content-Type", contentType(f, rate, 2))
			h.Set("Cache-Control", "no-cache, no-store")
			h.Set("Pragma", "no-cache")
			h.Set("Connection", "close")
			h.Set("icy-name", cfg.Name)
			h.Set("icy-description", cfg.Description)
			h.Set("icy-genre", cfg.Genre)
			h.Set("icy-pub", "0")
			if f.Name == "mp3" || f.Name == "opus" {
				h.Set("icy-br", strconv.Itoa(cfg.Bitrate))
			}
			h.Set("X-Audio-Sample-Rate", strconv.Itoa(rate))
			h.Set("X-Audio-Channels", "2")
			if wantMeta {
				h.Set("icy-metaint", strconv.Itoa(cfg.MetaInt))
			}
			// Without a deadline a stalled TCP peer (a player that stops
			// reading) parks this goroutine forever: it keeps its encoder,
			// its buffered chunks and its hub slot alive. The deadline is
			// refreshed before every flush below.
			rc := http.NewResponseController(w)
			setDeadline := func() {
				_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			}
			setDeadline()

			w.WriteHeader(http.StatusOK)
			flusher.Flush()

			ch := hub.AddListener()
			defer hub.RemoveListener(ch)
			log.Printf("listener + %s (%d, icy-meta=%v)", f.Name, hub.Listeners(), wantMeta)
			defer func() { log.Printf("listener - %s (%d)", f.Name, hub.Listeners()-1) }()

			var (
				sinceMeta int
				sentVer   uint64
				metaDue   = true // send the current title right after the first block
			)

			write := func(buf []byte) error {
				if len(buf) == 0 {
					return nil
				}
				st.with(func(s *stats) { s.BytesOut += int64(len(buf)) })
				if !wantMeta {
					_, err := w.Write(buf)
					return err
				}
				for len(buf) > 0 {
					n := cfg.MetaInt - sinceMeta
					if n > len(buf) {
						n = len(buf)
					}
					if _, err := w.Write(buf[:n]); err != nil {
						return err
					}
					buf = buf[n:]
					sinceMeta += n
					if sinceMeta < cfg.MetaInt {
						continue
					}
					sinceMeta = 0
					title, ver := hub.Title()
					block := []byte{0}
					if metaDue || ver != sentVer {
						block = icyMetaBlock(title, cfg.Name)
						sentVer, metaDue = ver, false
					}
					if _, err := w.Write(block); err != nil {
						return err
					}
				}
				return nil
			}

			if err := write(enc.Header()); err != nil {
				return
			}
			flusher.Flush()

			for {
				select {
				case chunk, ok := <-ch:
					if !ok {
						return
					}
					if chunk.FormatVer != formatVer {
						// source format changed: end the response so the
						// client reconnects and gets a fresh header
						return
					}
					out, err := enc.Encode(chunk.Samples)
					if err != nil {
						log.Printf("encode %s: %v", f.Name, err)
						return
					}
					if err := write(out); err != nil {
						return
					}
					flusher.Flush()
					setDeadline()
				case <-r.Context().Done():
					return
				}
			}
		}
	}

	// Only the configured formats get a route; everything else answers 404.
	notEnabled := func(w http.ResponseWriter) {
		http.Error(w, "format disabled; enabled: "+strings.Join(cfg.Formats, ", ")+
			" (set STREAM_FORMATS to change)", http.StatusNotFound)
	}
	for _, name := range formatNames() {
		f := formats[name]
		if _, ok := lookupFormat(name, cfg.Formats, cfg.DefaultFormat); ok {
			mux.HandleFunc(f.Path, streamHandler(f))
		} else {
			mux.HandleFunc(f.Path, func(w http.ResponseWriter, r *http.Request) {
				notEnabled(w)
			})
		}
	}

	// /stream?format=flac — defaults to DEFAULT_FORMAT
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		f, ok := lookupFormat(r.URL.Query().Get("format"), cfg.Formats, cfg.DefaultFormat)
		if !ok {
			notEnabled(w)
			return
		}
		streamHandler(f)(w, r)
	})

	mux.HandleFunc("/api/formats", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			ContentType string `json:"content_type"`
			ICY         bool   `json:"icy_metadata"`
			Description string `json:"description"`
			Enabled     bool   `json:"enabled"`
			Default     bool   `json:"default"`
		}
		out := []item{}
		for _, n := range formatNames() {
			f := formats[n]
			_, on := lookupFormat(n, cfg.Formats, cfg.DefaultFormat)
			out = append(out, item{f.Name, f.Path, f.ContentType, f.ICY, f.Desc,
				on, n == cfg.DefaultFormat})
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st.snapshot(cfg, hub.BufferedMs()))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		links := ""
		for _, n := range cfg.Formats {
			f := formats[n]
			links += fmt.Sprintf(`<li><a href="%s"><code>%s</code></a> — %s</li>`,
				f.Path, f.Path, f.Desc)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, statusPage, cfg.Name, cfg.Name, cfg.Bitrate,
			strings.Join(cfg.Formats, " · "), formats[cfg.DefaultFormat].Path, links)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:      mux,
		WriteTimeout: 0,
		IdleTimeout:  0,
	}
	log.Printf("http: :%d  (%s, /api/status)", cfg.HTTPPort,
		strings.Join(cfg.Formats, "/"))

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http: %v", err)
	}
}


const statusPage = `<!doctype html><html lang="de"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s — Sendspin Streamer</title>
<style>
 body{font:15px/1.6 ui-sans-serif,system-ui,sans-serif;background:#0d0f12;color:#e8e6e3;margin:0;padding:40px}
 main{max-width:640px;margin:auto}h1{font-size:22px;margin:0 0 4px}
 .m{color:#8b9099;font-size:13px}
 audio{width:100%%;margin:24px 0}
 table{width:100%%;border-collapse:collapse;font-size:14px}
 td{padding:6px 0;border-bottom:1px solid #1e232a}td:last-child{text-align:right;color:#8b9099}
 code{background:#171b21;padding:2px 6px;border-radius:4px}
</style>
<main>
<h1>%s</h1>
<div class="m">Sendspin player@v1 → PCM · %d kbps · formats: %s</div>
<audio controls preload="none" src="%s"></audio>

<table id="t"></table>
<p class="m">Streams:</p>
<ul class="m">%s</ul>
</main>

<script>
const rows=[["Titel","now_playing"],["Server","server"],["Wiedergabe","state"],["Quelle aktiv","streaming"],["Stream aktiv","output_active"],
["Samplerate","stream_sample_rate"],["Kanäle","stream_channels"],["Lautstärke","volume"],
["Stumm","muted"],["Puffer (ms)","buffer_ms"],["Underruns","underruns"]];
async function tick(){const s=await(await fetch('/api/status')).json();
 document.getElementById('t').innerHTML=rows.map(([l,k])=>{
  let v=s[k]; if(typeof v==='boolean')v=v?'ja':'nein';
  if(k==='output_active'){const n=s.listeners??0; v+=' ('+n+(n===1?' H\u00f6rer)':' H\u00f6rer)');}
  return '<tr><td>'+l+'</td><td>'+(v??'–')+'</td></tr>';}).join('');}
tick();setInterval(tick,2000);
</script></html>`
