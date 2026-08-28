// Package server is the CLI's built-in HTTP server.
//
// It does three jobs at once: it serves the image a node downloads while
// reflashing, it receives the card a node streams back during a capture, and
// — because every byte of both passes through it — it is also the progress
// meter the operator watches.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Route prefixes. Images are served under /i/ so a capture POST can never be
// confused with an image name.
const (
	ImagePrefix = "/i/"
	CapturePath = "/capture"

	// MACHeader mirrors the header the agent sets, so progress can be
	// attributed even when a proxy strips the query string.
	MACHeader = "X-Rasputin-Mac"
)

// Options configures a server.
type Options struct {
	// Port to listen on; 0 picks a free one.
	Port int
	// BindIP overrides LAN-IP detection. Tests set it to 127.0.0.1; in
	// production it is left empty and detected.
	BindIP net.IP
	// OutDir is where captures are staged and stored.
	OutDir string
	// Log receives one line per notable event; nil discards them.
	Log func(format string, args ...any)
}

// Server is a running HTTP server.
type Server struct {
	opts     Options
	listener net.Listener
	http     *http.Server
	baseURL  string

	mu       sync.Mutex
	images   map[string]string // name -> local path
	progress map[string]*Progress
	captures map[string]*capture
}

// Progress is what one client has pulled so far.
type Progress struct {
	// Client is the node's MAC when it reported one, otherwise its IP.
	Client string
	// MAC is the reported MAC, or "" if the node did not send one.
	MAC string
	// IP is the remote address the request came from.
	IP string
	// Image is the file being served.
	Image string
	// Bytes served so far, and Total for the whole file.
	Bytes int64
	Total int64
	// Started and Updated bracket the transfer.
	Started time.Time
	Updated time.Time
	// Done is set when the handler finished writing.
	Done bool
}

// Percent is how much of the image has been sent, 0 when the size is unknown.
func (p Progress) Percent() float64 {
	if p.Total <= 0 {
		return 0
	}
	return 100 * float64(p.Bytes) / float64(p.Total)
}

// Rate is the average throughput in bytes per second.
func (p Progress) Rate() float64 {
	d := p.Updated.Sub(p.Started).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(p.Bytes) / d
}

// Start binds a listener and begins serving.
func Start(opts Options) (*Server, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	if opts.OutDir == "" {
		opts.OutDir = "out"
	}
	ip := opts.BindIP
	if ip == nil {
		var err error
		if ip, err = LANIP(); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), fmt.Sprint(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("binding %s:%d: %w", ip, opts.Port, err)
	}

	s := &Server{
		opts:     opts,
		listener: ln,
		images:   map[string]string{},
		progress: map[string]*Progress{},
		captures: map[string]*capture{},
		baseURL:  "http://" + ln.Addr().String(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(ImagePrefix, s.serveImage)
	mux.HandleFunc(CapturePath, s.receiveCapture)
	s.http = &http.Server{Handler: mux}
	go func() {
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			opts.Log("http server stopped: %v", err)
		}
	}()
	opts.Log("serving on %s", s.baseURL)
	return s, nil
}

// BaseURL is the address nodes should use, e.g. http://192.168.0.228:8080.
func (s *Server) BaseURL() string { return s.baseURL }

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Close shuts the server down.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.http.Shutdown(ctx)
}

// Register makes a local file downloadable as ImagePrefix+name.
func (s *Server) Register(name, path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("registering %s: %w", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images[name] = path
	return nil
}

// URLFor is the URL a node should fetch for a registered image.
func (s *Server) URLFor(name string) string { return s.baseURL + ImagePrefix + name }

// CaptureURL is the URL a node should POST its card to.
func (s *Server) CaptureURL(id string) string {
	return fmt.Sprintf("%s%s?id=%s", s.baseURL, CapturePath, id)
}

// serveImage streams a registered file, counting bytes as it goes.
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, ImagePrefix)
	s.mu.Lock()
	path, ok := s.images[name]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "image unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "image unavailable", http.StatusInternalServerError)
		return
	}

	// HEAD is the agent's pre-write probe: answer it without starting a
	// transfer, and without creating a progress entry.
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/zstd")
		w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
		w.WriteHeader(http.StatusOK)
		return
	}

	p := s.beginProgress(r, name, info.Size())
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	cw := &countingWriter{w: w, onWrite: func(n int64) { s.addProgress(p, n) }}
	if _, err := copyTo(cw, f); err != nil {
		// A node that reboots mid-download is normal, not an error worth
		// failing on; the agent retries the whole stream.
		s.opts.Log("serving %s to %s stopped after %d bytes: %v", name, p.Client, cw.n, err)
	}
	s.finishProgress(p)
	s.opts.Log("served %s to %s (%d bytes)", name, p.Client, cw.n)
}

// clientKey identifies the node behind a request: its MAC if it reported one
// (a node keeps its MAC across a DHCP lease change), otherwise its IP.
func clientKey(r *http.Request) (key, mac, ip string) {
	mac = r.Header.Get(MACHeader)
	if mac == "" {
		mac = r.URL.Query().Get("mac")
	}
	mac = strings.ToLower(strings.TrimSpace(mac))
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if mac != "" {
		return mac, mac, ip
	}
	return ip, "", ip
}

func (s *Server) beginProgress(r *http.Request, image string, total int64) *Progress {
	key, mac, ip := clientKey(r)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &Progress{
		Client: key, MAC: mac, IP: ip, Image: image,
		Total: total, Started: now, Updated: now,
	}
	s.progress[key] = p
	return p
}

func (s *Server) addProgress(p *Progress, n int64) {
	s.mu.Lock()
	p.Bytes += n
	p.Updated = time.Now()
	s.mu.Unlock()
}

func (s *Server) finishProgress(p *Progress) {
	s.mu.Lock()
	p.Done = true
	p.Updated = time.Now()
	s.mu.Unlock()
}

// Progress returns a snapshot of every client's transfer, newest first.
func (s *Server) Progress() []Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Progress, 0, len(s.progress))
	for _, p := range s.progress {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// ProgressFor returns the transfer for one MAC or IP.
func (s *Server) ProgressFor(client string) (Progress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.progress[strings.ToLower(client)]
	if !ok {
		return Progress{}, false
	}
	return *p, true
}

// ResetProgress forgets past transfers, so a new flash starts from zero.
func (s *Server) ResetProgress() {
	s.mu.Lock()
	s.progress = map[string]*Progress{}
	s.mu.Unlock()
}

// LANIP finds the address other machines on the LAN can reach us on, by
// asking the kernel which source address it would use for an off-link
// destination. No packet is sent: a UDP "connection" only sets up routing.
func LANIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return nil, fmt.Errorf("detecting the LAN address: %w", err)
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("detecting the LAN address: unexpected address type %T", conn.LocalAddr())
	}
	if addr.IP == nil || addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		return nil, fmt.Errorf("detected the loopback address %v; the nodes could not reach this machine — is it on the LAN?", addr.IP)
	}
	return addr.IP, nil
}

// StagePath is where an in-flight capture is written before it is renamed
// into place.
func (s *Server) StagePath(id string) string {
	return filepath.Join(s.opts.OutDir, "incoming-"+id+".zst.tmp")
}

type countingWriter struct {
	w       http.ResponseWriter
	n       int64
	onWrite func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.onWrite != nil && n > 0 {
		c.onWrite(int64(n))
	}
	return n, err
}
