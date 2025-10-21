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

type ptySession struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}
}

func newHandler(srv *streamServer, title string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		html := strings.ReplaceAll(indexHTML, "{{TITLE}}", htmlEscape(title))
		_, _ = w.Write([]byte(html))
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

	server := &http.Server{Addr: addr, Handler: newHandler(srv, *title), ReadHeaderTimeout: 5 * time.Second}
	srvErr := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
		close(srvErr)
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
	c := exec.Command(args[0], args[1:]...)
	if os.Getenv("TERM") == "" {
		c.Env = append(os.Environ(), "TERM=xterm-256color")
	} else {
		c.Env = os.Environ()
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f, err := pty.Start(c)
	if err != nil {
		return nil, err
	}
	s := &ptySession{cmd: c, f: f, done: make(chan struct{})}

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

	go func() { _, _ = io.Copy(f, os.Stdin) }()

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
      })();
    </script>
  </body>
</html>`
