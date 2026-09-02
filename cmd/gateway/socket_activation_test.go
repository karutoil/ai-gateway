package main

// Socket-activation smoke test: the inheritListener helper must serve traffic
// from an fd passed the systemd way (fd 3 + LISTEN_PID/LISTEN_FDS env), and
// must fall back cleanly when the env is absent or malformed.

import (
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"testing"
)

// passFD dups fd onto 3 (systemd passes the listening socket as fd 3).
func passFD(t *testing.T, file *os.File) {
	t.Helper()
	if err := syscall.Dup3(int(file.Fd()), 3, 0); err != nil {
		t.Fatalf("dup3 to fd 3: %v", err)
	}
}

func TestInheritListenerServesInheritedSocket(t *testing.T) {
	// Clean any activation env leaked from another test.
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tcpLn := ln.(*net.TCPListener)
	file, err := tcpLn.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Act like systemd: LISTEN_PID = our pid, LISTEN_FDS = 1, socket on fd 3.
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "1")
	passFD(t, file)

	got, n := inheritListener()
	if got == nil {
		t.Fatal("inheritListener returned nil with a valid activation env")
	}
	if n != 1 {
		t.Fatalf("fds = %d, want 1", n)
	}

	// Serve and hit it.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "via-inherited-fd")
	})}
	go srv.Serve(got)
	defer srv.Close()
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-inherited-fd" {
		t.Fatalf("body = %q", body)
	}
}

func TestInheritListenerFallsBackWithoutEnv(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	ln, n := inheritListener()
	if ln != nil || n != 0 {
		t.Fatalf("without activation env: ln=%v n=%d, want nil/0", ln, n)
	}
}

func TestInheritListenerRejectsForeignPID(t *testing.T) {
	// LISTEN_PID naming a DIFFERENT process must be ignored (spec: guards
	// against a supervisor passing sockets meant for another child).
	t.Setenv("LISTEN_PID", "999999")
	t.Setenv("LISTEN_FDS", "1")
	ln, n := inheritListener()
	if ln != nil || n != 0 {
		t.Fatalf("foreign LISTEN_PID: ln=%v n=%d, want nil/0", ln, n)
	}
}
