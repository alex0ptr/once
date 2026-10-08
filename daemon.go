package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	idleGrace    = 30 * time.Second // lifetime of a daemon that never got an entry
	maxTimerWait = 30 * time.Second // timers stall during system sleep; recheck at least this often
)

type statusInfo struct {
	PID        int        `json:"pid"`
	Started    time.Time  `json:"started"`
	Entries    int        `json:"entries"`
	Bytes      int        `json:"bytes"`
	NextExpiry *time.Time `json:"next_expiry,omitempty"`
	ShutdownAt time.Time  `json:"shutdown_at"`
}

func runDaemon() int {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[daemon] ")
	disableCoreDumps()

	dir, err := runtimeDir()
	if err != nil {
		log.Printf("runtime dir: %v", err)
		return 1
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		log.Printf("lock file: %v", err)
		return 1
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0 // another daemon is already running
	}

	sock := socketPath(dir)
	_ = os.Remove(sock) // stale socket of a dead daemon; we hold the lock
	ln, err := net.Listen("unix", sock)
	if err != nil {
		log.Printf("listen: %v", err)
		return 1
	}
	_ = os.Chmod(sock, 0o600)
	defer os.Remove(sock)

	s := newStore(idleGrace)
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("serve: %v", err)
			s.shutdown()
		}
	}()
	log.Printf("started, pid %d, socket %s", os.Getpid(), sock)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	select {
	case <-s.done:
	case sig := <-sigs:
		log.Printf("received %v", sig)
		s.shutdown()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("stopped")
	return 0
}

type entry struct {
	data    []byte
	expires time.Time // wall clock, no monotonic reading
}

type store struct {
	mu      sync.Mutex
	entries map[string]*entry
	started time.Time
	grace   time.Duration
	next    time.Time // next expiry, or the end of the grace period if empty
	timer   *time.Timer
	stopped bool
	done    chan struct{}
}

func newStore(grace time.Duration) *store {
	now := time.Now()
	s := &store{
		entries: map[string]*entry{},
		started: now,
		grace:   grace,
		next:    now.Add(grace).Round(0),
		done:    make(chan struct{}),
	}
	// Hold the lock while assigning the timer, so the callback (which locks
	// before using s.timer) is guaranteed to see the assignment.
	s.mu.Lock()
	s.timer = time.AfterFunc(s.wait(), s.sweep)
	s.mu.Unlock()
	return s
}

// wait returns how long the timer sleeps until s.next. Go timers run on the
// monotonic clock, which stands still while the system sleeps, so the wait is
// capped: after a wake-up the wall clock is checked within maxTimerWait.
func (s *store) wait() time.Duration {
	return min(time.Until(s.next), maxTimerWait)
}

func (s *store) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/entries/{key}", s.handleGet)
	mux.HandleFunc("PUT /v1/entries/{key}", s.handlePut)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/clear", s.handleClear)
	return mux
}

func (s *store) get(key string, now time.Time) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if !now.Round(0).Before(e.expires) { // compare by wall clock
		wipe(e.data)
		delete(s.entries, key)
		return nil, false
	}
	return append([]byte(nil), e.data...), true
}

func (s *store) put(key string, data []byte, exp time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	if old, ok := s.entries[key]; ok {
		wipe(old.data)
	}
	s.entries[key] = &entry{data: data, expires: exp}
	if exp.Before(s.next) {
		s.next = exp
		s.timer.Reset(s.wait())
	}
	return true
}

// sweep drops expired entries, sets the timer to the next expiry and stops
// the daemon once nothing is left.
func (s *store) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	now := time.Now().Round(0)
	var next time.Time
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			wipe(e.data)
			delete(s.entries, k)
		} else if next.IsZero() || e.expires.Before(next) {
			next = e.expires
		}
	}
	if next.IsZero() {
		graceEnd := s.started.Add(s.grace).Round(0)
		if !now.Before(graceEnd) {
			s.stopLocked()
			return
		}
		next = graceEnd
	}
	s.next = next
	s.timer.Reset(s.wait())
}

func (s *store) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *store) stopLocked() {
	if s.stopped {
		return
	}
	s.stopped = true
	s.timer.Stop()
	for k, e := range s.entries {
		wipe(e.data)
		delete(s.entries, k)
	}
	close(s.done)
}

func (s *store) status() statusInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := statusInfo{PID: os.Getpid(), Started: s.started, Entries: len(s.entries)}
	for _, e := range s.entries {
		st.Bytes += len(e.data)
		if st.NextExpiry == nil || e.expires.Before(*st.NextExpiry) {
			st.NextExpiry = new(e.expires)
		}
		if e.expires.After(st.ShutdownAt) {
			st.ShutdownAt = e.expires
		}
	}
	if st.ShutdownAt.IsZero() {
		st.ShutdownAt = s.next // grace end
	}
	return st
}

// --- handlers -------------------------------------------------------------------

func (s *store) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !validKey(key) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	data, ok := s.get(key, time.Now())
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
	wipe(data)
}

func (s *store) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !validKey(key) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	exp, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("expires"))
	if err != nil {
		http.Error(w, "invalid expires", http.StatusBadRequest)
		return
	}
	if !exp.After(time.Now()) {
		http.Error(w, "expires lies in the past", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSize))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !s.put(key, data, exp) {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *store) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.status())
}

func (s *store) handleClear(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go s.shutdown()
}

// wipe overwrites cached secrets before they are dropped (best effort).
func wipe(b []byte) { clear(b) }
