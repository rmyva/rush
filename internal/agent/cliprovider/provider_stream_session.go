package cliprovider

import (
	"bufio"
	"context"
	"io"
	"sync"

	"charm.land/fantasy"
)

type procHandle struct {
	stdout    io.Reader
	usingPTY  bool
	kill      func()
	wait      func() (string, error)
	closeIO   func()
	drained   func()
	stderrRaw func() []byte
	cleanup   func()
}

type scanResult struct {
	raw  []byte
	done bool
	err  error
}

type StreamResult struct {
	Stderr  string
	Err     error
	ScanErr error
}

const (
	maxRetainedOutputBytes = 1024 * 1024
	outputTruncationMarker = "\n...(output truncated; retained output capped)\n"
)

type StreamSession struct {
	stream          fantasy.StreamResponse
	done            chan struct{}
	notify          chan struct{}
	mu              sync.Mutex
	lines           []scanResult
	output          []byte
	outputTruncated bool
	result          StreamResult
	stopOnce        sync.Once
	stopDone        chan struct{}
	proc            procHandle
}

func (s *StreamSession) Stream() fantasy.StreamResponse { return s.stream }
func (s *StreamSession) Done() <-chan struct{}          { return s.done }
func (s *StreamSession) Output() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.output...)
}

func (s *StreamSession) Wait(ctx context.Context) (StreamResult, error) {
	select {
	case <-s.done:
		return s.result, nil
	case <-ctx.Done():
		return StreamResult{}, ctx.Err()
	}
}

func (s *StreamSession) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		go func() { s.proc.kill(); close(s.stopDone) }()
	})
	select {
	case <-s.stopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *StreamSession) kill() {
	s.stopOnce.Do(func() { go func() { s.proc.kill(); close(s.stopDone) }() })
}

func (s *StreamSession) legacyKill() {
	s.kill()
	<-s.stopDone
}

func (s *StreamSession) ready(index int) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < len(s.lines) {
		ready := make(chan struct{})
		close(ready)
		return ready
	}
	return s.notify
}

func (s *StreamSession) next(ctx context.Context, index int) (scanResult, bool) {
	for {
		s.mu.Lock()
		if index < len(s.lines) {
			line := s.lines[index]
			s.mu.Unlock()
			return line, true
		}
		notify := s.notify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return scanResult{}, false
		}
	}
}

func newStreamSession(proc procHandle) *StreamSession {
	s := &StreamSession{done: make(chan struct{}), notify: make(chan struct{}), stopDone: make(chan struct{}), proc: proc}
	go func() {
		scanner := bufio.NewScanner(io.TeeReader(proc.stdout, streamCapture{s}))
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			raw := append([]byte(nil), scanner.Bytes()...)
			s.mu.Lock()
			s.lines = append(s.lines, scanResult{raw: raw})
			close(s.notify)
			s.notify = make(chan struct{})
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.lines = append(s.lines, scanResult{done: true, err: scanner.Err()})
		close(s.notify)
		s.notify = make(chan struct{})
		s.mu.Unlock()
		if scanner.Err() != nil && !proc.usingPTY {
			s.kill()
			if closer, ok := proc.stdout.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		if proc.drained != nil {
			proc.drained()
		}
		stderr, err := proc.wait()
		if proc.stderrRaw != nil {
			s.mu.Lock()
			s.appendOutput(proc.stderrRaw())
			s.mu.Unlock()
		}
		s.result = StreamResult{Stderr: stderr, Err: err, ScanErr: scanner.Err()}
		if proc.cleanup != nil {
			proc.cleanup()
		}
		close(s.done)
	}()
	return s
}

type streamCapture struct{ session *StreamSession }

func (s *StreamSession) appendOutput(p []byte) {
	if s.outputTruncated || len(p) == 0 {
		return
	}
	marker := []byte(outputTruncationMarker)
	remaining := maxRetainedOutputBytes - len(s.output)
	if len(p) <= remaining {
		s.output = append(s.output, p...)
		return
	}

	// Reserve marker space, but retain as much as possible from this final
	// write rather than discarding the entire chunk that crossed the cap.
	keep := maxRetainedOutputBytes - len(marker)
	if len(s.output) > keep {
		s.output = s.output[:keep]
	}
	available := keep - len(s.output)
	if available > len(p) {
		available = len(p)
	}
	s.output = append(s.output, p[:available]...)
	s.output = append(s.output, marker...)
	s.outputTruncated = true
}

func (c streamCapture) Write(p []byte) (int, error) {
	c.session.mu.Lock()
	c.session.appendOutput(p)
	c.session.mu.Unlock()
	return len(p), nil
}
