package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxSandboxOutput = 30_000
	outputHeadBytes  = 12_000
	outputTailBytes  = 12_000
)

// liveSandbox is one long-running shell inside a bwrap namespace. Commands
// are sent over anonymous pipes, so sandbox processes cannot forge protocol
// state by editing files in a shared IPC directory.
type liveSandbox struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	done   chan struct{}

	closeOnce sync.Once
	stopping  atomic.Bool
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (l *liveSandbox) alive() bool {
	if l == nil || l.cmd == nil || l.cmd.Process == nil || l.stopping.Load() {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func (l *liveSandbox) closePipes() {
	l.closeOnce.Do(func() {
		if l.stdin != nil {
			_ = l.stdin.Close()
		}
		if l.stdout != nil {
			_ = l.stdout.Close()
		}
	})
}

func (l *liveSandbox) stop() {
	if l == nil {
		return
	}
	l.stopping.Store(true)
	// Killing the bwrap monitor triggers --die-with-parent for the sandboxed
	// command. Closing the pipes also unblocks an in-flight protocol read.
	if l.cmd != nil && l.cmd.Process != nil {
		_ = l.cmd.Process.Kill()
	}
	l.closePipes()
}

func (m *managedSession) liveAlive() bool {
	return m.live.alive()
}

func (m *managedSession) stopLive() {
	live := m.live
	m.live = nil
	if live != nil {
		live.stop()
	}
	clearLegacyIPC(m.paths.Run)
}

func clearLegacyIPC(runDir string) {
	for _, name := range []string{
		"supervisor.sh", "job.sh", "start", "done", "exit", "cancel",
		"pid", "ready", "stdout", "stderr", "supervisor.log",
	} {
		_ = os.Remove(filepath.Join(runDir, name))
	}
}

func (s *apiServer) ensureLive(managed *managedSession, spec RunSpec) error {
	if managed.liveAlive() {
		return nil
	}
	managed.stopLive()
	if err := os.MkdirAll(managed.paths.Run, 0o700); err != nil {
		return err
	}
	clearLegacyIPC(managed.paths.Run)
	argv, err := s.sandbox.liveArgv(spec)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create sandbox stdin: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return fmt.Errorf("create sandbox stdout: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(managed.paths.Run, "sandbox.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return err
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logFile.Close()
		return fmt.Errorf("start sandbox: %w", err)
	}
	// The child inherited its copies during Start.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	live := &liveSandbox{
		cmd:    cmd,
		stdin:  stdinW,
		stdout: stdoutR,
		done:   make(chan struct{}),
	}
	managed.live = live
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		close(live.done)
		managed.mu.Lock()
		if managed.live == live {
			live.closePipes()
			managed.live = nil
		}
		managed.mu.Unlock()
	}()

	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := runLiveJob(probeCtx, live, ":"); err != nil {
		managed.stopLive()
		return fmt.Errorf("sandbox did not become ready: %w", err)
	}
	return nil
}

type shellResult struct {
	Stdout string
	Stderr string
	Exit   int
}

func runLiveJob(ctx context.Context, live *liveSandbox, command string) (shellResult, error) {
	if !live.alive() {
		return shellResult{}, fmt.Errorf("sandbox is not running")
	}
	token, err := randomID()
	if err != nil {
		return shellResult{}, fmt.Errorf("create command marker: %w", err)
	}
	marker := []byte("\x1ePIGO_DONE_" + token + ":")
	// The command gets /dev/null as stdin so it cannot consume the persistent
	// shell's control stream. A separate sh -c also prevents cd/export/exit
	// from mutating or terminating that shell.
	script := "/bin/sh -c " + shQuote(command) + " </dev/null 2>&1\n" +
		"__pigo_status=$?\n" +
		"printf '\\036PIGO_DONE_" + token + ":%d\\037\\n' \"$__pigo_status\"\n"

	cancelWatcher := context.AfterFunc(ctx, live.stop)
	if _, err := io.WriteString(live.stdin, script); err != nil {
		cancelWatcher()
		live.stop()
		if ctx.Err() != nil {
			return shellResult{}, ctx.Err()
		}
		return shellResult{}, fmt.Errorf("write sandbox command: %w", err)
	}

	var output boundedSandboxOutput
	code, readErr := readCommandResult(live.stdout, marker, &output)
	watcherStopped := cancelWatcher()
	result := shellResult{Stdout: output.String(), Exit: code}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if !watcherStopped {
		return result, context.Canceled
	}
	if readErr != nil {
		live.stop()
		return result, readErr
	}
	return result, nil
}

func readCommandResult(r io.Reader, marker []byte, output io.Writer) (int, error) {
	buf := make([]byte, 32*1024)
	pending := make([]byte, 0, len(marker)+32)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := bytes.Index(pending, marker)
				if idx >= 0 {
					if idx > 0 {
						_, _ = output.Write(pending[:idx])
					}
					status := pending[idx+len(marker):]
					end := bytes.IndexByte(status, '\x1f')
					if end < 0 {
						// A forged/incomplete marker must not retain unbounded
						// output while waiting for a terminator.
						if len(status) > 16 {
							_, _ = output.Write(pending[:idx+1])
							pending = append(pending[:0], pending[idx+1:]...)
							continue
						}
						pending = pending[idx:]
						break
					}
					code, err := strconv.Atoi(string(status[:end]))
					if err != nil {
						return 0, fmt.Errorf("invalid sandbox exit status: %w", err)
					}
					return code, nil
				}

				keep := len(marker) - 1
				if len(pending) <= keep {
					break
				}
				flush := len(pending) - keep
				_, _ = output.Write(pending[:flush])
				pending = append(pending[:0], pending[flush:]...)
				break
			}
		}
		if readErr != nil {
			if len(pending) > 0 {
				_, _ = output.Write(pending)
			}
			if readErr == io.EOF {
				return 0, fmt.Errorf("sandbox exited before reporting command status")
			}
			return 0, fmt.Errorf("read sandbox output: %w", readErr)
		}
	}
}

type boundedSandboxOutput struct {
	full      []byte
	head      []byte
	tail      []byte
	truncated bool
}

func (w *boundedSandboxOutput) Write(p []byte) (int, error) {
	n := len(p)
	if !w.truncated {
		w.full = append(w.full, p...)
		if len(w.full) <= maxSandboxOutput {
			return n, nil
		}
		w.truncated = true
		w.head = append(w.head, w.full[:outputHeadBytes]...)
		start := len(w.full) - outputTailBytes
		w.tail = append(w.tail, w.full[start:]...)
		w.full = nil
		return n, nil
	}
	w.tail = append(w.tail, p...)
	if len(w.tail) > outputTailBytes {
		w.tail = append(w.tail[:0], w.tail[len(w.tail)-outputTailBytes:]...)
	}
	return n, nil
}

func (w *boundedSandboxOutput) String() string {
	if !w.truncated {
		return string(w.full)
	}
	return string(w.head) + "\n[truncated]\n" + string(w.tail)
}

func (s *apiServer) reapLoop() {
	if s.config.idleTimeout <= 0 && s.config.emptySessionTTL <= 0 {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.reaperStop:
			return
		case <-ticker.C:
			s.reapIdle()
		}
	}
}

func (s *apiServer) reapIdle() {
	s.mu.RLock()
	list := make([]*managedSession, 0, len(s.sessions))
	for _, m := range s.sessions {
		list = append(list, m)
	}
	s.mu.RUnlock()
	now := time.Now()
	for _, m := range list {
		if !m.mu.TryLock() {
			continue
		}
		idle := s.config.idleTimeout > 0 && now.Sub(m.meta.LastUsed) >= s.config.idleTimeout
		if idle && m.activeTurn() == nil && m.liveAlive() {
			m.stopLive()
		}
		id := m.meta.ID
		m.mu.Unlock()
		if s.config.emptySessionTTL > 0 {
			s.removeExpiredEmptySession(id, m, now)
		}
	}
}
