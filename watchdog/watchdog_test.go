package watchdog

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNotify_NoSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Notify("READY=1"); err != nil {
		t.Fatalf("Notify without socket: %v", err)
	}
}

func TestNotify_WritesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("NOTIFY_SOCKET", path)

	if err := Notify("READY=1"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	buf := make([]byte, 64)
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := ln.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
}

func TestInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	t.Setenv("WATCHDOG_PID", "")
	if Interval() != 0 {
		t.Fatal("unset WATCHDOG_USEC should disable")
	}
	t.Setenv("WATCHDOG_USEC", "30000000")
	if got := Interval(); got != 30*time.Second {
		t.Fatalf("got %v", got)
	}
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))
	if Interval() != 0 {
		t.Fatal("watchdog meant for another PID should disable")
	}
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if Interval() != 30*time.Second {
		t.Fatal("own PID should enable")
	}
}

func TestMonitor_WithholdsKeepaliveWhenLoopStalls(t *testing.T) {
	now := time.Unix(1000, 0)
	var sent int
	m := &Monitor{
		notify: func(string) error { sent++; return nil },
		now:    func() time.Time { return now },
	}
	fw := m.Register("firewall", 5*time.Minute)
	disp := m.Register("dispatcher", 5*time.Minute)

	if !m.tick() || sent != 1 {
		t.Fatal("fresh loops should send keepalive")
	}

	// Dispatcher keeps beating, firewall does not.
	now = now.Add(6 * time.Minute)
	disp.last.Store(now.UnixNano())
	if m.tick() || sent != 1 {
		t.Fatal("stalled firewall loop should withhold keepalive")
	}
	if s := m.stale(); len(s) != 1 || s[0] != "firewall" {
		t.Fatalf("stale = %v", s)
	}

	// Firewall recovers.
	fw.last.Store(now.UnixNano())
	if !m.tick() || sent != 2 {
		t.Fatal("recovered loops should resume keepalive")
	}
}

func TestHeartbeat_NilIsNoop(t *testing.T) {
	var h *Heartbeat
	h.Beat()
}
