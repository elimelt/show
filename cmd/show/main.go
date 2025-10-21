// show streams stdin to an HTTP page rendering a terminal.
// Author: Elijah Melton
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

import (
	pty "github.com/creack/pty"
	"golang.org/x/term"
)

type event struct {
	data   []byte
	offset int64
}

type streamServer struct {
	mu      sync.RWMutex
	history []byte
	base    int64
	total   int64
	maxHist int
	clients map[chan event]struct{}
	done    bool
	doneCh  chan struct{}
}

func newStreamServer(maxHistory int) *streamServer {
	return &streamServer{
		maxHist: maxHistory,
		clients: make(map[chan event]struct{}),
		doneCh:  make(chan struct{}),
	}
}

func (s *streamServer) append(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.mu.Lock()
	s.history = append(s.history, chunk...)
	s.total += int64(len(chunk))
	if s.maxHist > 0 && len(s.history) > s.maxHist {
		drop := len(s.history) - s.maxHist
		if drop < len(s.history) {
			newHist := make([]byte, s.maxHist)
			copy(newHist, s.history[drop:])
			s.history = newHist
			s.base += int64(drop)
		} else {
			s.base += int64(len(s.history))
			s.history = s.history[:0]
		}
	}
	off := s.total
	cpy := make([]byte, len(chunk))
	copy(cpy, chunk)
	dests := make([]chan event, 0, len(s.clients))
	for ch := range s.clients {
		dests = append(dests, ch)
	}
	s.mu.Unlock()

	ev := event{data: cpy, offset: off}
	for _, ch := range dests {
		select {
		case ch <- ev:
		default:
			s.removeClient(ch)
		}
	}
}

func (s *streamServer) addClient(ch chan event) {
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
}

func (s *streamServer) removeClient(ch chan event) {
	s.mu.Lock()
	delete(s.clients, ch)
	s.mu.Unlock()
}

func (s *streamServer) finish() {
	s.mu.Lock()
	if !s.done {
		s.done = true
		close(s.doneCh)
	}
	s.mu.Unlock()
}

func (s *streamServer) isDone() bool { s.mu.RLock(); d := s.done; s.mu.RUnlock(); return d }

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func writeSSEChunk(w http.ResponseWriter, flusher http.Flusher, id int64, payload []byte) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Grow(32 + len(payload)*4/3 + 16)
	buf.WriteString("id: ")
	buf.WriteString(strconv.FormatInt(id, 10))
	buf.WriteString("\n")
	buf.WriteString("event: chunk\n")
	buf.WriteString("data: ")
	b64 := base64.NewEncoder(base64.StdEncoding, buf)
	_, _ = b64.Write(payload)
	_ = b64.Close()
	buf.WriteString("\n\n")
	_, _ = w.Write(buf.Bytes())
	buf.Reset()
	bufPool.Put(buf)
	flusher.Flush()
}

var version = "dev"

type streamWriter struct{ s *streamServer }

func (w streamWriter) Write(p []byte) (int, error) { w.s.append(p); return len(p), nil }

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type ptySession struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}
	in   *lockedWriter
}

func newHandler(srv *streamServer, title string, allowInput *bool, input *io.Writer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		html := strings.ReplaceAll(indexHTML, "{{TITLE}}", htmlEscape(title))
		enabled := "false"
		if allowInput != nil && *allowInput && input != nil {
			if *input != nil {
				enabled = "true"
			}
		}
		html = strings.ReplaceAll(html, "{{INPUT}}", enabled)
		_, _ = w.Write([]byte(html))
	})
	mux.HandleFunc("/input", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || allowInput == nil || !*allowInput || input == nil || *input == nil {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 64))
		if len(b) > 0 {
			_, _ = (*input).Write(b)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch := make(chan event, 256)
		srv.addClient(ch)
		defer srv.removeClient(ch)

		_, _ = w.Write([]byte("retry: 2000\n\n"))
		flusher.Flush()

		var startAbs int64
		if last := r.Header.Get("Last-Event-ID"); last != "" {
			if n, err := strconv.ParseInt(last, 10, 64); err == nil && n >= 0 {
				startAbs = n
			}
		}
		srv.mu.RLock()
		base := srv.base
		cur := len(srv.history)
		curAbs := base + int64(cur)
		doneNow := srv.done
		if startAbs < base {
			startAbs = base
		}
		if startAbs > curAbs {
			startAbs = curAbs
		}
		startIndex := int(startAbs - base)
		if startIndex < cur {
			window := make([]byte, cur-startIndex)
			copy(window, srv.history[startIndex:])
			srv.mu.RUnlock()
			const step = 32 * 1024
			for pos := 0; pos < len(window); pos += step {
				n := pos + step
				if n > len(window) {
					n = len(window)
				}
				writeSSEChunk(w, flusher, base+int64(startIndex+n), window[pos:n])
			}
		} else {
			srv.mu.RUnlock()
		}
		if doneNow {
			_, _ = w.Write([]byte("event: done\n"))
			_, _ = w.Write([]byte("data: 1\n\n"))
			flusher.Flush()
			return
		}

		if srv.isDone() {
			_, _ = w.Write([]byte("event: done\n"))
			_, _ = w.Write([]byte("data: 1\n\n"))
			flusher.Flush()
			return
		}

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-srv.doneCh:
				_, _ = w.Write([]byte("event: done\n"))
				_, _ = w.Write([]byte("data: 1\n\n"))
				flusher.Flush()
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": ping\n\n"))
				flusher.Flush()
			case ev := <-ch:
				writeSSEChunk(w, flusher, ev.offset, ev.data)
			}
		}
	})
	return mux
}

func main() {
	port := flag.Int("p", 8000, "Port to listen on")
	host := flag.String("host", "127.0.0.1", "Host/interface to bind")
	title := flag.String("title", "show", "Page title")
	historyBytes := flag.Int64("history", 16<<20, "Max bytes to retain for replay; 0 = unlimited")
	showVersion := flag.Bool("version", false, "Print version and exit")
	ptyMode := flag.Bool("pty", false, "Run a command under a PTY and stream it")
	allowInput := flag.Bool("input", false, "Allow browser keyboard input (PTY mode)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	addr := fmt.Sprintf("%s:%d", *host, *port)
	maxHist := int(*historyBytes)
	if *historyBytes < 0 {
		maxHist = 0
	}
	srv := newStreamServer(maxHist)

	var input io.Writer
	server := &http.Server{Addr: addr, Handler: newHandler(srv, *title, allowInput, &input), ReadHeaderTimeout: 5 * time.Second}
	srvErr := make(chan error, 1)
	// Listen on requested address; if it's loopback IPv4, also try loopback IPv6.
	var listeners []net.Listener
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	listeners = append(listeners, ln)
	if *host == "127.0.0.1" || *host == "localhost" {
		// Try IPv6 loopback on the same port.
		v6 := fmt.Sprintf("[::1]:%d", *port)
		if ln6, err := net.Listen("tcp", v6); err == nil {
			listeners = append(listeners, ln6)
		}
	}
	go func() {
		for _, l := range listeners {
			go func(li net.Listener) {
				if err := server.Serve(li); err != nil && err != http.ErrServerClosed {
					srvErr <- err
				}
			}(l)
		}
	}()

	var sess *ptySession
	if *ptyMode {
		args := flag.Args()
		if len(args) == 0 {
			log.Fatalf("pty mode requires a command: show -pty -- <cmd> [args...]")
		}
		var err error
		sess, err = startPTY(srv, args)
		if err != nil {
			log.Fatalf("pty: %v", err)
		}
		input = sess.in
	} else {
		go func() {
			reader := bufio.NewReader(os.Stdin)
			mw := io.MultiWriter(os.Stdout, streamWriter{s: srv})
			_, _ = io.CopyBuffer(mw, reader, make([]byte, 4096))
			srv.finish()
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		if sess != nil && sess.cmd != nil && sess.cmd.Process != nil {
			_ = sess.cmd.Process.Signal(syscall.SIGINT)
		}
		_ = server.Close()
	case <-func() chan struct{} {
		if sess != nil {
			return sess.done
		}
		return make(chan struct{})
	}():
		_ = server.Close()
	case err := <-srvErr:
		if err != nil {
			log.Fatalf("server error: %v", err)
		}
	}
}

// PTY integration for interactive commands.
func startPTY(srv *streamServer, args []string) (*ptySession, error) {
	var c *exec.Cmd
	if len(args) == 1 {
		c = exec.Command("/bin/sh", "-c", args[0])
	} else {
		c = exec.Command(args[0], args[1:]...)
	}
	if os.Getenv("TERM") == "" {
		c.Env = append(os.Environ(), "TERM=xterm-256color")
	} else {
		c.Env = os.Environ()
	}
	f, err := pty.Start(c)
	if err != nil {
		return nil, err
	}
	lw := &lockedWriter{w: f}
	s := &ptySession{cmd: c, f: f, done: make(chan struct{}), in: lw}

	if isatty(os.Stdout.Fd()) {
		if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
		}
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		go func() {
			for range ch {
				if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
				}
			}
		}()
	}

	var oldState *term.State
	if isatty(os.Stdin.Fd()) {
		if st, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			oldState = st
		}
	}

	go func() { _, _ = io.Copy(lw, os.Stdin) }()

	mw := io.MultiWriter(os.Stdout, streamWriter{s: srv})
	go func() {
		_, _ = io.CopyBuffer(mw, f, make([]byte, 4096))
		srv.finish()
		if oldState != nil {
			_ = term.Restore(int(os.Stdin.Fd()), oldState)
		}
		_ = f.Close()
		close(s.done)
	}()
	return s, nil
}

func isatty(fd uintptr) bool {
	return term.IsTerminal(int(fd))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

const indexHTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{{TITLE}}</title>
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.css" />
    <style>
      html, body { height: 100%; margin: 0; background: #000; }
      #terminal { position: fixed; inset: 0; }
      .xterm, .xterm * {
        font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas,
          "Liberation Mono", "DejaVu Sans Mono", "Ubuntu Mono", "Courier New", monospace;
      }
    </style>
  </head>
  <body>
    <div id="terminal"></div>
    <script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.7.0/lib/xterm-addon-fit.min.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/xterm-addon-unicode11@0.4.0/lib/xterm-addon-unicode11.min.js"></script>
    <script>
      (function(){
        const term = new window.Terminal({
          cursorBlink: false,
          convertEol: true,
          disableStdin: true,
          scrollback: 100000,
          theme: { background: '#000000' }
        });
        const fitAddon = new window.FitAddon.FitAddon();
        term.loadAddon(fitAddon);
        try {
          const unicode11 = new window.Unicode11Addon.Unicode11Addon();
          term.loadAddon(unicode11);
          term.unicode.activeVersion = '11';
        } catch (e) { /* addon optional */ }
        const el = document.getElementById('terminal');
        term.open(el);
        function fit() { try { fitAddon.fit(); } catch (e) {} }
        window.addEventListener('resize', fit);
        fit();

        function b64ToBytes(b64) {
          const s = atob(b64);
          const out = new Uint8Array(s.length);
          for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
          return out;
        }
        const sseDecoder = new TextDecoder('utf-8', { fatal: false });
        function writeBytes(bytes) {
          const text = sseDecoder.decode(bytes, { stream: true });
          if (text) term.write(text);
        }

        const es = new EventSource('/stream');
        es.addEventListener('chunk', (ev) => {
          try {
            writeBytes(b64ToBytes(ev.data));
            term.scrollToBottom();
          } catch (e) {}
        });
        es.addEventListener('done', () => { es.close(); });

        const INPUT_ENABLED = {{INPUT}};
        if (INPUT_ENABLED) {
          const enc = new TextEncoder();
          function send(bytes) { fetch('/input', { method: 'POST', body: bytes }).catch(() => {}); }
          function ctrl(ch) { const c = ch.toUpperCase().charCodeAt(0) - 64; return (c >= 0 && c <= 31) ? c : null; }
          function keyToBytes(ev) {
            if (ev.ctrlKey && ev.key && ev.key.length === 1) {
              const c = ctrl(ev.key);
              if (c !== null) return new Uint8Array([c]);
            }
            switch (ev.key) {
              case 'Enter': return enc.encode('\r');
              case 'Backspace': return new Uint8Array([0x7f]);
              case 'Tab': return enc.encode('\t');
              case 'Escape': return new Uint8Array([0x1b]);
              case 'ArrowUp': return enc.encode('\x1b[A');
              case 'ArrowDown': return enc.encode('\x1b[B');
              case 'ArrowRight': return enc.encode('\x1b[C');
              case 'ArrowLeft': return enc.encode('\x1b[D');
              default:
                if (ev.key && ev.key.length === 1) return enc.encode(ev.key);
            }
            return null;
          }
          document.addEventListener('keydown', (ev) => {
            const b = keyToBytes(ev);
            if (b) { ev.preventDefault(); send(b); }
          });
        }
      })();
    </script>
  </body>
</html>`
