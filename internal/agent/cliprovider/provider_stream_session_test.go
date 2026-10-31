package cliprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestStreamOutputRetentionIsBounded(t *testing.T) {
	s := &StreamSession{}
	s.appendOutput([]byte("prefix"))
	chunk := strings.Repeat("z", maxRetainedOutputBytes)
	s.appendOutput([]byte(chunk))

	got := s.Output()
	keep := maxRetainedOutputBytes - len(outputTruncationMarker)
	want := "prefix" + chunk[:keep-len("prefix")] + outputTruncationMarker
	if string(got) != want {
		t.Fatalf("retained output does not preserve the prefix of the final chunk: length=%d", len(got))
	}

	s.appendOutput([]byte("later write"))
	again := s.Output()
	if len(again) != maxRetainedOutputBytes || string(again) != want {
		t.Fatalf("retained output changed on writes after truncation: length=%d", len(again))
	}
}

func TestRetainedStreamLifetime(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%v", pty), func(t *testing.T) {
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			reader, writer := io.Pipe()
			waitGate := make(chan struct{})
			waitStarted := make(chan struct{})
			drainedStarted := make(chan struct{})
			drainedGate := make(chan struct{})
			killGate := make(chan struct{})
			killStarted := make(chan struct{})
			var kills, cleanups, visible atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("late exit")
			m := &cliModel{spec: CLISpec{Binary: self, NoPTY: !pty, BuildArgs: func(bool) []string { return nil }}, streamProcess: func(noPTY bool, _ []string) procHandle {
				if noPTY != (!pty || testDisablePTY) {
					t.Fatal("production_mode_selection")
				}
				return procHandle{
					stdout: reader, usingPTY: pty,
					kill:      func() { kills.Add(1); close(killStarted); <-killGate },
					wait:      func() (string, error) { close(waitStarted); <-waitGate; return "late stderr", sentinel },
					cleanup:   func() { cleanups.Add(1) },
					drained:   func() { close(drainedStarted); <-drainedGate },
					stderrRaw: func() []byte { return []byte(" late stderr\r\n") },
				}
			}, streamVisibleCleanup: func() { visible.Add(1) }}
			s, err := m.StartStream(ctx, fantasy.Call{})
			if err != nil {
				t.Fatal(err)
			}
			output := strings.Repeat("line\n", 100) + "final\n"
			written := make(chan struct{})
			go func() { _, _ = io.WriteString(writer, output); close(written) }()
			awaitRetained(t, written, "drain_without_iterator")
			select {
			case <-s.Done():
				t.Fatal("done_before_io_and_wait")
			default:
			}
			iterationDone := make(chan struct{})
			iterationStarted := make(chan struct{})
			go func() {
				s.Stream()(func(part fantasy.StreamPart) bool {
					if part.Type == fantasy.StreamPartTypeTextStart {
						close(iterationStarted)
					}
					return true
				})
				close(iterationDone)
			}()
			awaitRetained(t, iterationStarted, "iteration_started")
			cancel()
			awaitRetained(t, killStarted, "cancel_dispatch")
			select {
			case <-iterationDone:
				t.Fatal("legacy_kill_before_iterator_return")
			default:
			}
			if visible.Load() != 0 {
				t.Fatal("legacy_kill_before_visible_cleanup")
			}
			select {
			case <-s.Done():
				t.Fatal("cancel_is_not_termination")
			default:
			}
			if cleanups.Load() != 0 {
				t.Fatal("physical_cleanup_before_termination")
			}
			control, stopCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer stopCancel()
			if !errors.Is(s.Stop(control), context.DeadlineExceeded) {
				t.Fatal("bounded_stop_timeout")
			}
			select {
			case <-iterationDone:
				t.Fatal("legacy_kill_before_iterator_return")
			case <-time.After(30 * time.Millisecond):
			}
			if visible.Load() != 0 {
				t.Fatal("legacy_kill_before_visible_cleanup")
			}
			close(killGate)
			awaitRetained(t, iterationDone, "iterator_cancel_bounded")
			if visible.Load() != 1 {
				t.Fatal("visible_cleanup_timing")
			}
			late := "late\r\nunterminated"
			if _, err := io.WriteString(writer, late); err != nil {
				t.Fatal(err)
			}
			output += late
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kills.Load() != 1 {
				t.Fatal("cancel_dispatch_exactly_once")
			}
			_ = writer.Close()
			awaitRetained(t, drainedStarted, "drained_gate_entered")
			select {
			case <-waitStarted:
				t.Fatal("wait_before_drained_io")
			default:
			}
			select {
			case <-s.Done():
				t.Fatal("done_before_drained_io")
			default:
			}
			if cleanups.Load() != 0 {
				t.Fatal("cleanup_before_drained_io")
			}
			close(drainedGate)
			awaitRetained(t, waitStarted, "wait_after_io")
			select {
			case <-s.Done():
				t.Fatal("held_wait_keeps_done_open")
			default:
			}
			close(waitGate)
			awaitRetained(t, s.Done(), "late_wait_completion")
			result, err := s.Wait(context.Background())
			if err != nil || result.Err != sentinel || result.Stderr != "late stderr" {
				t.Fatal("late_wait_result_retained")
			}
			if string(s.Output()) != output+" late stderr\r\n" {
				t.Fatal("final_output_retained")
			}
			if cleanups.Load() != 1 {
				t.Fatal("physical_cleanup_exactly_once")
			}
			for range s.Stream() {
			}
			if visible.Load() != 1 || cleanups.Load() != 1 {
				t.Fatal("repeat_cleanup_exactly_once")
			}
		})
	}
}

func TestRetainedStreamEvents(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%v", pty), func(t *testing.T) {
			m := &cliModel{spec: CLISpec{Binary: self, BuildArgs: func(bool) []string { return nil }}, streamProcess: func(bool, []string) procHandle {
				return procHandle{stdout: strings.NewReader("hello\nfinal\n"), usingPTY: pty, kill: func() {}, wait: func() (string, error) { return "", nil }}
			}}
			stream, err := m.Stream(context.Background(), fantasy.Call{})
			if err != nil {
				t.Fatal(err)
			}
			var parts []fantasy.StreamPart
			for part := range stream {
				parts = append(parts, part)
			}
			if len(parts) != 5 || parts[0].Type != fantasy.StreamPartTypeTextStart || parts[1].Delta != "hello\n" || parts[2].Delta != "final\n" || parts[3].Type != fantasy.StreamPartTypeTextEnd || parts[4].Type != fantasy.StreamPartTypeFinish {
				t.Fatal("legacy_stream_events")
			}
			response, err := m.Generate(context.Background(), fantasy.Call{})
			if err != nil || response.Content.Text() != "hello\nfinal\n" {
				t.Fatal("legacy_generate_result")
			}
		})
	}
}

func TestRetainedStreamYieldFalse(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var kills, cleanups atomic.Int32
	m := &cliModel{spec: CLISpec{Binary: self, BuildArgs: func(bool) []string { return nil }}, streamProcess: func(bool, []string) procHandle {
		return procHandle{stdout: strings.NewReader("tail\n"), kill: func() { kills.Add(1) }, wait: func() (string, error) { return "", nil }, cleanup: func() { cleanups.Add(1) }}
	}}
	s, err := m.StartStream(ctx, fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	s.Stream()(func(fantasy.StreamPart) bool { return false })
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitRetained(t, s.Done(), "yield_false_completion")
	if kills.Load() != 1 || cleanups.Load() != 1 || string(s.Output()) != "tail\n" {
		t.Fatal("yield_false_retains_lifetime")
	}
}

func awaitRetained(t *testing.T, done <-chan struct{}, assertion string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(assertion)
	}
}
