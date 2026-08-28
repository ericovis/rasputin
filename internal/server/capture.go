package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// captureBufferSize matches the agent's copy buffer: this is a multi-gigabyte
// stream and the default 32 KiB would mean a lot of syscalls.
const captureBufferSize = 4 << 20

// capture is one expected upload.
type capture struct {
	id       string
	destPath string

	mu        sync.Mutex
	inFlight  bool
	result    *CaptureResult
	done      chan struct{}
	closeDone sync.Once
}

// CaptureResult describes a finished upload.
type CaptureResult struct {
	ID       string
	MAC      string
	Path     string
	Bytes    int64
	SHA256   string
	Duration time.Duration
	Err      error
}

// ExpectCapture registers an id a node may POST to, and where the finished
// upload should end up. Only a registered id is accepted, so a stray POST
// cannot write anywhere.
func (s *Server) ExpectCapture(id, destPath string) (<-chan struct{}, error) {
	if id == "" {
		return nil, fmt.Errorf("capture id is required")
	}
	if destPath == "" {
		return nil, fmt.Errorf("capture destination is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.captures[id]; exists {
		return nil, fmt.Errorf("capture %s is already registered", id)
	}
	c := &capture{id: id, destPath: destPath, done: make(chan struct{})}
	s.captures[id] = c
	return c.done, nil
}

// CaptureResult returns the outcome of a registered capture, if it finished.
func (s *Server) CaptureResult(id string) (*CaptureResult, bool) {
	s.mu.Lock()
	c, ok := s.captures[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.result == nil {
		return nil, false
	}
	return c.result, true
}

// receiveCapture streams a node's card to disk.
//
// The upload is written to a staging file and only renamed into place after
// a clean EOF and an fsync, so a half-received card can never be mistaken
// for a golden image. The node is told 200 only once that rename succeeded.
func (s *Server) receiveCapture(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "capture requires POST", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	_, mac, ip := clientKey(r)

	s.mu.Lock()
	c, ok := s.captures[id]
	s.mu.Unlock()
	if !ok {
		s.opts.Log("rejected a capture POST for unknown id %q from %s", id, ip)
		http.Error(w, "unknown capture id", http.StatusNotFound)
		return
	}

	// Single-flight: a node that retries while a previous attempt is still
	// streaming must not have both writing the same staging file.
	c.mu.Lock()
	if c.inFlight {
		c.mu.Unlock()
		http.Error(w, "a capture for this id is already in progress", http.StatusConflict)
		return
	}
	if c.result != nil && c.result.Err == nil {
		c.mu.Unlock()
		// Already complete: the node's previous attempt succeeded and it
		// simply did not hear the answer. Say yes again.
		w.WriteHeader(http.StatusOK)
		return
	}
	c.inFlight = true
	c.mu.Unlock()

	start := time.Now()
	res := &CaptureResult{ID: id, MAC: mac, Path: c.destPath}
	stage := s.StagePath(id)
	res.Bytes, res.SHA256, res.Err = s.streamToFile(r.Body, stage)

	if res.Err == nil {
		res.Err = os.Rename(stage, c.destPath)
	}
	if res.Err != nil {
		os.Remove(stage)
		s.opts.Log("capture %s from %s FAILED after %d bytes: %v", id, mac, res.Bytes, res.Err)
	}
	res.Duration = time.Since(start)

	c.mu.Lock()
	c.inFlight = false
	c.result = res
	c.mu.Unlock()

	if res.Err != nil {
		http.Error(w, res.Err.Error(), http.StatusInternalServerError)
		// The node retries, so the channel is only closed on success.
		return
	}
	s.opts.Log("capture %s from %s complete: %d bytes in %s -> %s",
		id, mac, res.Bytes, res.Duration.Round(time.Second), c.destPath)
	w.WriteHeader(http.StatusOK)
	c.closeDone.Do(func() { close(c.done) })
}

// streamToFile writes body to path, returning the byte count and hash.
func (s *Server) streamToFile(body io.Reader, path string) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, h), body, make([]byte, captureBufferSize))
	if err != nil {
		return n, "", fmt.Errorf("receiving the capture after %d bytes: %w", n, err)
	}
	if err := f.Sync(); err != nil {
		return n, "", fmt.Errorf("syncing the capture: %w", err)
	}
	if err := f.Close(); err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// copyTo is io.Copy with the large buffer image serving wants.
func copyTo(dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(dst, src, make([]byte, captureBufferSize))
}
