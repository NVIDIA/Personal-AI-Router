// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// child is one mlx_lm.server process, pinned to a single model for its lifetime.
//
// One model per process rather than reusing a process and switching models: a
// switch inside mlx_lm.server is exactly the unload/reload this component exists
// to avoid, and a process that only ever holds one model can be evicted by
// killing it, which releases the weights deterministically. mlx-lm has no
// "unload" API to call instead.
type child struct {
	model string
	port  int
	cmd   *exec.Cmd
	used  time.Time // LRU position; touched on every request routed here
	inuse int       // in-flight requests; a child is never evicted while > 0
	// exit is closed-by-send when cmd.Wait returns. One waiter goroutine per
	// child, started at spawn: calling cmd.Wait twice is an error, and both the
	// readiness check and the stop path need to know when the process is gone.
	exit chan error
}

// exitCh returns a channel that yields once the process has been reaped.
// Buffered and single-shot, so a second read (ready-check then stop) still
// returns rather than blocking forever.
func (c *child) exitCh() <-chan error {
	if c.exit == nil {
		ch := make(chan error, 1)
		close(ch)
		return ch
	}
	return c.exit
}

// Pool keeps up to max children alive, evicting the least recently used.
//
// Two locks on purpose, because one lock is a liveness bug:
//
//   - mu guards the maps and is held only for map operations. /health reads
//     through it, so it answers in microseconds even while a 27B is loading.
//     Holding a single lock across the load instead would block the health
//     probe for minutes, and PAIR would conclude the engine had died and
//     restart it -- killing the very load it was waiting for.
//   - loadMu serialises the slow path. Two large models loading at once on a
//     memory-bound machine is the thing most likely to take the box down, and
//     serialising them also makes the cap trivially correct: only one goroutine
//     is ever evicting-then-spawning.
//
// inflight deduplicates concurrent requests for the same cold model, so ten
// simultaneous first-requests produce one process, not ten.
type Pool struct {
	mu       sync.Mutex
	children map[string]*child
	inflight map[string]*loadOp
	freed    *sync.Cond // signalled when a child's last request finishes
	max      int

	loadMu sync.Mutex

	serverBin   string
	serverFlags []string
	portBase    int
	readyWait   time.Duration
}

// loadOp is one in-progress load that later arrivals wait on rather than repeat.
type loadOp struct {
	done chan struct{}
	port int
	err  error
}

func NewPool(serverBin string, serverFlags []string, max, portBase int, readyWait time.Duration) *Pool {
	if max < 1 {
		max = 1
	}
	p := &Pool{
		children: map[string]*child{}, inflight: map[string]*loadOp{},
		max: max, serverBin: serverBin, serverFlags: serverFlags,
		portBase: portBase, readyWait: readyWait,
	}
	p.freed = sync.NewCond(&p.mu)
	return p
}

// Resident lists the models currently held, most recently used first. This is
// what /health reports and therefore what PAIR's routing keys on.
func (p *Pool) Resident() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*child, 0, len(p.children))
	for _, c := range p.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].used.After(out[j].used) })
	models := make([]string, len(out))
	for i, c := range out {
		models[i] = c.model
	}
	return models
}

// Acquire returns the port serving model and a release func the caller MUST
// call when its request finishes. The child is pinned against eviction until
// then, so a streaming response cannot be terminated by a concurrent request
// for a different model.
func (p *Pool) Acquire(ctx context.Context, model string) (int, func(), error) {
	// Fast path: already resident. Takes mu only.
	p.mu.Lock()
	if c, ok := p.children[model]; ok {
		c.used = time.Now()
		c.inuse++
		p.mu.Unlock()
		return c.port, p.releaser(model), nil
	}
	// Someone else is already loading exactly this model: wait for them.
	if op, ok := p.inflight[model]; ok {
		p.mu.Unlock()
		select {
		case <-op.done:
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
		if op.err != nil {
			return 0, nil, op.err
		}
		return p.Acquire(ctx, model)
	}
	op := &loadOp{done: make(chan struct{})}
	p.inflight[model] = op
	p.mu.Unlock()

	// Deliberately NOT the request's context. A 27B can take minutes; a client
	// that gives up must not abort the load, or a model slow enough to time out
	// could never finish loading -- every attempt would kill the last one's
	// progress. The work is shared: later arrivals join through inflight above,
	// and readyWait still bounds it.
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), p.readyWait)
	port, err := p.load(loadCtx, model)
	cancelLoad()

	p.mu.Lock()
	op.port, op.err = port, err
	delete(p.inflight, model)
	close(op.done)
	p.mu.Unlock()

	if err != nil {
		return 0, nil, err
	}
	return p.Acquire(ctx, model)
}

func (p *Pool) releaser(model string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if c, ok := p.children[model]; ok {
				if c.inuse > 0 {
					c.inuse--
				}
				// Recency is "last touched", not "last admitted": a generation
				// that ran for minutes must not come out of it looking older
				// than a model that has been idle throughout.
				c.used = time.Now()
			}
			p.freed.Broadcast()
			p.mu.Unlock()
		})
	}
}

// load evicts if necessary and spawns. Serialised by loadMu: the caller has
// already claimed model in inflight, so only distinct models reach here at once.
func (p *Pool) load(ctx context.Context, model string) (int, error) {
	p.loadMu.Lock()
	defer p.loadMu.Unlock()

	for {
		p.mu.Lock()
		if len(p.children) < p.max {
			p.mu.Unlock()
			break
		}
		victim := p.evictableLocked()
		if victim == nil {
			// Every resident model is mid-request. Waiting is right: killing a
			// child to serve a new model would abort someone's in-flight
			// generation, and at max-models=1 that is the common case rather
			// than a corner one.
			slog.Info("pool full and every model is busy; waiting for a request to finish",
				"want", model, "max_models", p.max)
			if err := p.waitFreeLocked(ctx); err != nil {
				p.mu.Unlock()
				return 0, err
			}
			p.mu.Unlock()
			continue
		}
		slog.Info("evicting least recently used model", "model", victim.model, "port", victim.port,
			"idle", time.Since(victim.used).Round(time.Second), "want", model, "max_models", p.max)
		p.mu.Unlock()
		// Outside mu: stopping waits for the process to exit, which must not
		// block /health. It is still inside loadMu, so no other load races it.
		p.stop(victim)
	}

	port, err := p.freePort()
	if err != nil {
		return 0, err
	}
	c, err := p.spawn(ctx, model, port)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	p.children[model] = c
	p.mu.Unlock()
	return port, nil
}

// waitFreeLocked blocks until a request finishes. Caller holds mu; it is
// released while waiting. ctx cancellation is surfaced by a watchdog broadcast.
func (p *Pool) waitFreeLocked(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.freed.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	p.freed.Wait()
	return ctx.Err()
}

func (p *Pool) evictableLocked() *child {
	var oldest *child
	for _, c := range p.children {
		if c.inuse > 0 {
			continue
		}
		if oldest == nil || c.used.Before(oldest.used) {
			oldest = c
		}
	}
	return oldest
}

// freePort picks a port for a new child. The bind test is advisory -- another
// process can take it between the check and the child's own bind -- so a child
// that fails to start is retried on the next candidate by the caller's error
// path rather than treated as fatal.
func (p *Pool) freePort() (int, error) {
	p.mu.Lock()
	taken := make(map[int]bool, len(p.children))
	for _, c := range p.children {
		taken[c.port] = true
	}
	p.mu.Unlock()
	for port := p.portBase; port < p.portBase+64; port++ {
		if taken[port] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		l.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port in [%d,%d)", p.portBase, p.portBase+64)
}

func (p *Pool) spawn(ctx context.Context, model string, port int) (*child, error) {
	args := append([]string{
		"--model", model, "--host", "127.0.0.1", "--port", fmt.Sprint(port),
	}, p.serverFlags...)

	cmd := exec.Command(p.serverBin, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr // the child's logs are ours; our stdout stays clean
	cmd.Stderr = os.Stderr
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mlx_lm.server for %q: %w", model, err)
	}
	slog.Info("loading model", "model", model, "port", port, "pid", cmd.Process.Pid)

	c := &child{model: model, port: port, cmd: cmd, used: time.Now()}
	// Surface an early exit (a bad model id, a missing file) instead of waiting
	// out the whole ready timeout on a process that is already gone.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	c.exit = exited

	if err := waitReady(ctx, port, p.readyWait, exited); err != nil {
		p.stop(c)
		return nil, fmt.Errorf("model %q did not become ready: %w", model, err)
	}
	slog.Info("model ready", "model", model, "port", port)
	return c, nil
}

// stop terminates a child and waits for it, so its memory is released before a
// replacement is spawned. Without the wait, an eviction made to free room could
// overlap the load it made room for -- the one moment both models are resident,
// on a machine that by definition could not hold both.
func (p *Pool) stop(c *child) {
	p.mu.Lock()
	delete(p.children, c.model)
	p.mu.Unlock()

	if c.cmd == nil || c.cmd.Process == nil {
		return
	}
	terminate(c.cmd)
	select {
	case <-c.exitCh():
	case <-time.After(30 * time.Second):
		slog.Warn("model did not exit in time; killing", "model", c.model, "pid", c.cmd.Process.Pid)
		_ = c.cmd.Process.Kill()
		<-c.exitCh()
	}
	slog.Info("stopped model", "model", c.model, "port", c.port)
}

// Unload releases one model's weights on demand, without waiting for another
// load to evict it.
//
// mlx-lm itself has no unload API -- which is why the engine long advertised no
// Eject at all -- but a pool child holds exactly one model for its lifetime, so
// ending the process IS the unload, and it frees the memory deterministically.
//
// Refuses while requests are in flight rather than killing mid-stream: the same
// rule eviction follows, and a user asking to free memory does not expect it to
// truncate somebody's answer. Unloading a model that is not resident is not an
// error -- the caller wanted it gone, and it is.
func (p *Pool) Unload(model string) error {
	p.mu.Lock()
	c, ok := p.children[model]
	if !ok {
		p.mu.Unlock()
		return nil
	}
	if c.inuse > 0 {
		inuse := c.inuse
		p.mu.Unlock()
		return fmt.Errorf("model %q is serving %d request(s); it will unload when they finish", model, inuse)
	}
	p.mu.Unlock()

	// stop() takes mu itself to unlink the child, then waits for the process to
	// exit outside the lock -- so /health keeps answering while a 27B tears down.
	p.stop(c)
	return nil
}

// StopAll tears the pool down so no orphan is left holding GPU memory.
func (p *Pool) StopAll() {
	p.mu.Lock()
	all := make([]*child, 0, len(p.children))
	for _, c := range p.children {
		all = append(all, c)
	}
	p.mu.Unlock()
	for _, c := range all {
		p.stop(c)
	}
}

func waitReady(ctx context.Context, port int, within time.Duration, exited <-chan error) error {
	deadline := time.Now().Add(within)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("process exited before becoming ready: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no /health within %s", within)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
