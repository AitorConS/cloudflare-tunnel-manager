// Package runner supervises local cloudflared connector processes: one per
// managed tunnel, restarted with backoff, with logs kept in memory.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	StateStopped    = "stopped"
	StateRunning    = "running"
	StateRestarting = "restarting"
)

type Status struct {
	State     string `json:"state"`
	StartedAt int64  `json:"startedAt"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"lastError"`
}

type Manager struct {
	bin   string
	mu    sync.Mutex
	procs map[int64]*proc
	logs  map[int64]*LogBuffer
}

func NewManager(bin string) *Manager {
	return &Manager{bin: bin, procs: map[int64]*proc{}, logs: map[int64]*LogBuffer{}}
}

// BinaryVersion returns the cloudflared version string, or an error when the
// binary is not installed.
func (m *Manager) BinaryVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, m.bin, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type proc struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	status      Status
	metricsAddr string
}

func (p *proc) set(f func(*Status)) {
	p.mu.Lock()
	f(&p.status)
	p.mu.Unlock()
}

// Logs returns the (lazily created) log buffer of a tunnel. It outlives the
// process so logs remain readable after a stop or crash.
func (m *Manager) Logs(id int64) *LogBuffer {
	m.mu.Lock()
	defer m.mu.Unlock()
	lb := m.logs[id]
	if lb == nil {
		lb = &LogBuffer{subs: map[chan Line]struct{}{}}
		m.logs[id] = lb
	}
	return lb
}

// Start launches the connector for the tunnel; it is a no-op when already running.
func (m *Manager) Start(id int64, token string) {
	logs := m.Logs(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.procs[id]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{cancel: cancel, done: make(chan struct{})}
	p.status.State = StateRestarting
	m.procs[id] = p
	go m.supervise(ctx, p, token, logs)
}

// Stop terminates the connector and waits for it to exit.
func (m *Manager) Stop(id int64) {
	m.mu.Lock()
	p := m.procs[id]
	delete(m.procs, id)
	m.mu.Unlock()
	if p != nil {
		p.cancel()
		<-p.done
	}
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]int64, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); m.Stop(id) }()
	}
	wg.Wait()
}

func (m *Manager) Status(id int64) Status {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return Status{State: StateStopped}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

func (m *Manager) supervise(ctx context.Context, p *proc, token string, logs *LogBuffer) {
	defer close(p.done)
	backoff := time.Second
	for {
		started := time.Now()
		err := m.runOnce(ctx, p, token, logs)
		if ctx.Err() != nil {
			logs.Add("[manager] connector stopped")
			return
		}
		msg := "cloudflared exited unexpectedly"
		if err != nil {
			msg = err.Error()
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		logs.Add(fmt.Sprintf("[manager] %s; retrying in %s", msg, backoff))
		p.set(func(s *Status) {
			s.State, s.LastError = StateRestarting, msg
			s.Restarts++
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (m *Manager) runOnce(ctx context.Context, p *proc, token string, logs *LogBuffer) error {
	addr, err := freeAddr()
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(m.bin, "tunnel", "--no-autoupdate", "--metrics", addr, "run")
	// The token goes through the environment so it never shows up in `ps`.
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return fmt.Errorf("could not start cloudflared: %w", err)
	}
	w.Close()
	logs.Add("[manager] cloudflared started (pid " + strconv.Itoa(cmd.Process.Pid) + ")")
	p.mu.Lock()
	p.status.State, p.status.StartedAt, p.metricsAddr = StateRunning, time.Now().Unix(), addr
	p.mu.Unlock()

	exited := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			logs.Add(sc.Text())
		}
		r.Close()
		exited <- cmd.Wait()
	}()

	select {
	case err := <-exited:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		return nil
	}
}

// Metrics scrapes the connector's Prometheus endpoint. Samples of the same
// metric are summed across labels; `byLabel` additionally keeps the per-label
// breakdown for the requested metric/label pairs (e.g. response codes).
type Metrics struct {
	Totals  map[string]float64            `json:"totals"`
	ByLabel map[string]map[string]float64 `json:"byLabel"`
}

func (m *Manager) Metrics(ctx context.Context, id int64, breakdown map[string]string) (*Metrics, error) {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	addr, state := p.metricsAddr, p.status.State
	p.mu.Unlock()
	if state != StateRunning || addr == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseMetrics(resp.Body, breakdown), nil
}

func parseMetrics(r io.Reader, breakdown map[string]string) *Metrics {
	out := &Metrics{Totals: map[string]float64{}, ByLabel: map[string]map[string]float64{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		val, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			continue
		}
		name, labels := line[:sp], ""
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name, labels = name[:i], strings.TrimSuffix(name[i+1:], "}")
		}
		if !strings.HasPrefix(name, "cloudflared_") && !strings.HasPrefix(name, "quic_") {
			continue
		}
		out.Totals[name] += val
		if label, ok := breakdown[name]; ok {
			if v := labelValue(labels, label); v != "" {
				if out.ByLabel[name] == nil {
					out.ByLabel[name] = map[string]float64{}
				}
				out.ByLabel[name][v] += val
			}
		}
	}
	return out
}

func labelValue(labels, key string) string {
	for _, part := range strings.Split(labels, ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// ---- logs ----

type Line struct {
	Seq  int64  `json:"seq"`
	Time int64  `json:"t"`
	Text string `json:"text"`
}

const maxLines = 1000

type LogBuffer struct {
	mu    sync.Mutex
	lines []Line
	seq   int64
	subs  map[chan Line]struct{}
}

func (b *LogBuffer) Add(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	l := Line{Seq: b.seq, Time: time.Now().UnixMilli(), Text: text}
	b.lines = append(b.lines, l)
	if len(b.lines) > maxLines {
		b.lines = append(b.lines[:0], b.lines[len(b.lines)-maxLines:]...)
	}
	for ch := range b.subs {
		select {
		case ch <- l:
		default: // slow subscriber: drop rather than block cloudflared's output
		}
	}
}

// Subscribe returns the current backlog plus a channel with later lines.
func (b *LogBuffer) Subscribe() ([]Line, chan Line) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Line, 256)
	b.subs[ch] = struct{}{}
	return append([]Line(nil), b.lines...), ch
}

func (b *LogBuffer) Unsubscribe(ch chan Line) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}
