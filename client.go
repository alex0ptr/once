package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	baseURL = "http://once"
	maxSize = 64 << 20 // largest output that is cached
)

func runCached(args []string) int {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors and usage are printed below
	fs.Usage = func() {}
	ttl := fs.Duration("ttl", 0, "")
	untilS := fs.String("until", "", "")
	tenant := fs.String("tenant", "", "")
	refresh := fs.Bool("refresh", false, "")
	noDir := fs.Bool("no-dir", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return 0
		}
		return usageErrorf("%v", err)
	}
	argv := fs.Args()
	switch {
	case len(argv) == 0:
		return usageErrorf("no command given")
	case *ttl <= 0:
		return usageErrorf("--ttl is required and must be positive")
	}
	if *tenant == "" {
		*tenant = os.Getenv("ONCE_TENANT")
	}
	if *tenant == "" {
		return usageErrorf("a tenant key is required (--tenant or $ONCE_TENANT)")
	}
	var until time.Time
	if *untilS != "" {
		t, err := parseUntil(*untilS)
		if err != nil {
			return usageErrorf("%v", err)
		}
		until = t
	}

	dir, err := runtimeDir()
	if err != nil {
		warnf("runtime dir: %v", err)
		return execOnly(argv)
	}
	scope := "" // --no-dir: the same entry for every directory
	if !*noDir {
		cwd, err := os.Getwd()
		if err != nil {
			warnf("cannot determine working directory: %v", err)
			return 1
		}
		scope = cwd
	}
	key := cacheKey(*tenant, scope, argv)
	c := newClient(socketPath(dir))

	if !*refresh {
		data, ok, err := getEntry(c, key)
		if err != nil && !isDialErr(err) {
			warnf("cache lookup failed: %v", err)
		}
		if ok {
			if _, err := os.Stdout.Write(data); err != nil {
				return 1
			}
			return 0
		}
	}

	code, out, overflow := execute(argv)
	if code != 0 {
		return code
	}
	if overflow {
		warnf("output larger than %d MiB, not cached", maxSize>>20)
		return 0
	}
	exp := expiry(time.Now(), *ttl, until)
	if !exp.After(time.Now()) {
		warnf("--until lies in the past, result not cached")
		return 0
	}
	if err := storeResult(c, dir, key, out, exp); err != nil {
		warnf("could not cache result: %v", err)
	}
	return 0
}

// execOnly runs the command without any caching (fallback on setup errors).
func execOnly(argv []string) int {
	code, _, _ := execute(argv)
	return code
}

// execute runs argv in the current directory with the caller's environment.
// stdout is passed through and captured; stdin and stderr are inherited so
// interactive prompts keep working.
func execute(argv []string) (code int, out []byte, overflow bool) {
	buf := &capBuffer{limit: maxSize}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = io.MultiWriter(os.Stdout, buf)

	// The child receives terminal signals (Ctrl-C) itself; we just stay alive
	// to report its exit status. SIGTERM/SIGHUP are forwarded.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		warnf("%v", err)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return 127, nil, false
		}
		return 126, nil, false
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM || s == syscall.SIGHUP {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)

	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), nil, false
			}
			return ee.ExitCode(), nil, false
		}
		return 1, nil, false // e.g. stdout closed by the reader
	}
	return 0, buf.Bytes(), buf.overflow
}

// capBuffer captures up to limit bytes and never fails a write, so a large
// output never breaks the pass-through to stdout.
type capBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if !b.overflow {
		if b.Len()+len(p) > b.limit {
			b.overflow = true
			b.Reset()
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// --- daemon communication ---------------------------------------------------

func newClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func isDialErr(err error) bool {
	op, ok := errors.AsType[*net.OpError](err)
	return ok && op.Op == "dial"
}

func getEntry(c *http.Client, key string) ([]byte, bool, error) {
	resp, err := c.Get(baseURL + "/v1/entries/" + key)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		return data, err == nil, err
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("daemon: %s", resp.Status)
	}
}

func putEntry(c *http.Client, key string, data []byte, exp time.Time) error {
	u := baseURL + "/v1/entries/" + key + "?expires=" + url.QueryEscape(exp.Format(time.RFC3339Nano))
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("daemon: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// store makes sure a daemon is running and hands it the result. If the daemon
// shuts down between the check and the upload, it retries once.
func storeResult(c *http.Client, dir, key string, data []byte, exp time.Time) error {
	var err error
	for range 2 {
		if err = ensureDaemon(c, dir); err != nil {
			continue
		}
		if err = putEntry(c, key, data, exp); err == nil {
			return nil
		}
	}
	return err
}

func ping(c *http.Client) error {
	resp, err := c.Get(baseURL + "/v1/status")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ensureDaemon starts a detached daemon (re-executing this binary) if none
// answers on the socket. Concurrent starts are harmless: the daemon holds an
// exclusive flock and any second instance exits immediately.
func ensureDaemon(c *http.Client, dir string) error {
	if ping(c) == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(exe, "__daemon")
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = daemonEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ping(c) == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("daemon did not start (see " + filepath.Join(dir, "daemon.log") + ")")
}

// daemonEnv passes only what the daemon needs; in particular the tenant key
// never reaches the daemon.
func daemonEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "ONCE_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// --- subcommands ---------------------------------------------------------------

func runStatus() int {
	dir, err := runtimeDir()
	if err != nil {
		warnf("runtime dir: %v", err)
		return 1
	}
	c := newClient(socketPath(dir))
	resp, err := c.Get(baseURL + "/v1/status")
	if err != nil {
		if isDialErr(err) {
			fmt.Println("daemon:      not running")
			return 1
		}
		warnf("%v", err)
		return 1
	}
	defer resp.Body.Close()
	var st statusInfo
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		warnf("bad status response: %v", err)
		return 1
	}
	now := time.Now()
	fmt.Printf("daemon:      running (pid %d, since %s)\n", st.PID, st.Started.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("entries:     %d (%s)\n", st.Entries, humanBytes(st.Bytes))
	if st.NextExpiry != nil {
		fmt.Printf("next expiry: %s\n", when(*st.NextExpiry, now))
	}
	fmt.Printf("shuts down:  %s\n", when(st.ShutdownAt, now))
	fmt.Printf("socket:      %s\n", socketPath(dir))
	return 0
}

func runClear() int {
	dir, err := runtimeDir()
	if err != nil {
		warnf("runtime dir: %v", err)
		return 1
	}
	c := newClient(socketPath(dir))
	resp, err := c.Post(baseURL+"/v1/clear", "", nil)
	if err != nil {
		if isDialErr(err) {
			fmt.Println("daemon not running, nothing to clear")
			return 0
		}
		warnf("%v", err)
		return 1
	}
	resp.Body.Close()
	fmt.Println("cache cleared, daemon stopped")
	return 0
}

func when(t, now time.Time) string {
	d := max(t.Sub(now).Round(time.Second), 0)
	return fmt.Sprintf("%s (in %s)", t.Local().Format("2006-01-02 15:04:05"), d)
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
