// Package watchdog speaks systemd's notify protocol: READY/STOPPING, and a
// WATCHDOG=1 keepalive that is only sent while every registered loop is still
// making progress. A loop that wedges stops beating, the keepalive stops, and
// systemd kills and restarts the agent, so a hang becomes a crash.
package watchdog

import (
	"context"
	"cs-agent/log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Notify sends one state line (e.g. "READY=1") to systemd. It is a no-op when
// the process was not started by systemd with a notify socket.
func Notify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:] // abstract socket
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// Interval returns systemd's watchdog timeout for this process, or 0 when the
// watchdog is not enabled (WATCHDOG_USEC unset, or meant for another PID).
func Interval() time.Duration {
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	return time.Duration(usec) * time.Microsecond
}

// Heartbeat is one monitored loop. A nil *Heartbeat is valid and Beat is a no-op,
// so components work unchanged without a monitor (tests, CLI subcommands).
type Heartbeat struct {
	name   string
	maxAge time.Duration
	last   atomic.Int64 // unix nanos
}

// Beat records that the loop made progress.
func (h *Heartbeat) Beat() {
	if h == nil {
		return
	}
	h.last.Store(time.Now().UnixNano())
}

func (h *Heartbeat) age(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, h.last.Load()))
}

// Monitor gates the systemd keepalive on its registered heartbeats.
type Monitor struct {
	mu    sync.Mutex
	beats []*Heartbeat
	// notify and now are overridable in tests.
	notify func(string) error
	now    func() time.Time
}

func New() *Monitor {
	return &Monitor{notify: Notify, now: time.Now}
}

// Register adds a loop that must Beat at least every maxAge. The loop starts
// fresh, so it has maxAge from registration to produce its first beat.
func (m *Monitor) Register(name string, maxAge time.Duration) *Heartbeat {
	h := &Heartbeat{name: name, maxAge: maxAge}
	h.last.Store(m.now().UnixNano())
	m.mu.Lock()
	m.beats = append(m.beats, h)
	m.mu.Unlock()
	return h
}

// stale returns the names of loops that have not beaten within their maxAge.
func (m *Monitor) stale() []string {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, h := range m.beats {
		if h.age(now) > h.maxAge {
			out = append(out, h.name)
		}
	}
	return out
}

// tick sends the keepalive if every loop is fresh. It reports whether it did.
func (m *Monitor) tick() bool {
	if s := m.stale(); len(s) > 0 {
		log.New().Error("watchdog: loop stalled; withholding keepalive so systemd restarts the agent",
			"loops", strings.Join(s, ","))
		return false
	}
	if err := m.notify("WATCHDOG=1"); err != nil {
		log.New().Warn("watchdog: notify failed", "error", err.Error())
	}
	return true
}

// Run pings systemd at half the watchdog timeout until ctx is done. It returns
// immediately when systemd's watchdog is not enabled for this process.
func (m *Monitor) Run(ctx context.Context) {
	iv := Interval()
	if iv <= 0 {
		return
	}
	t := time.NewTicker(iv / 2)
	defer t.Stop()
	m.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick()
		}
	}
}
