package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const supervisorScript = `#!/bin/sh
IPC="${IPC:-/run/pigo-ipc}"
export HOME="${HOME:-/home/pigo}"
export PIGO_HOME="${PIGO_HOME:-/home/pigo/.pigo}"
export PATH="${PATH:-/tmp:/usr/local/bin:/usr/bin:/bin}"
export USER="${USER:-pigo}"
export TERM="${TERM:-dumb}"
cd /workspace 2>/dev/null || cd "${WORKSPACE:-.}" || true
touch "$IPC/ready"
while true; do
  if [ -f "$IPC/start" ]; then
    rm -f "$IPC/start" "$IPC/done" "$IPC/exit" "$IPC/cancel"
    sh "$IPC/job.sh" > "$IPC/stdout" 2> "$IPC/stderr" &
    job=$!
    echo "$job" > "$IPC/pid"
    while kill -0 "$job" 2>/dev/null; do
      if [ -f "$IPC/cancel" ]; then
        kill -9 "$job" 2>/dev/null
        break
      fi
      sleep 0.05
    done
    wait "$job" 2>/dev/null
    echo $? > "$IPC/exit"
    touch "$IPC/done"
  else
    sleep 0.1
  fi
done
`

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func writeSupervisor(runDir string) error {
	return os.WriteFile(filepath.Join(runDir, "supervisor.sh"), []byte(supervisorScript), 0o700)
}

func writeShellJob(runDir, script string) error {
	if !strings.HasPrefix(script, "#!") {
		script = "#!/bin/sh\n" + script
	}
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	return os.WriteFile(filepath.Join(runDir, "job.sh"), []byte(script), 0o700)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func (m *managedSession) liveAlive() bool {
	if m.live == nil || m.live.Process == nil {
		return false
	}
	return processAlive(m.live.Process.Pid)
}

func (m *managedSession) stopLive() {
	if m.live != nil {
		killProcessGroup(m.live)
		m.live = nil
	}
	clearIPC(m.paths.Run)
}

func clearIPC(runDir string) {
	for _, name := range []string{"start", "done", "exit", "cancel", "pid", "ready", "stdout"} {
		_ = os.Remove(filepath.Join(runDir, name))
	}
}

func (s *apiServer) ensureLive(managed *managedSession, spec RunSpec) error {
	if managed.liveAlive() {
		return nil
	}
	managed.live = nil
	if err := os.MkdirAll(managed.paths.Run, 0o700); err != nil {
		return err
	}
	clearIPC(managed.paths.Run)
	if err := writeSupervisor(managed.paths.Run); err != nil {
		return err
	}
	argv, err := s.sandbox.liveArgv(spec, managed.paths.Run)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logFile, err := os.OpenFile(filepath.Join(managed.paths.Run, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start sandbox: %w", err)
	}
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		managed.mu.Lock()
		if managed.live == cmd {
			managed.live = nil
		}
		managed.mu.Unlock()
	}()
	managed.live = cmd
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(managed.paths.Run, "ready")); err == nil {
			return nil
		}
		if !processAlive(cmd.Process.Pid) {
			managed.stopLive()
			return fmt.Errorf("sandbox exited before becoming ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	managed.stopLive()
	return fmt.Errorf("sandbox supervisor did not become ready")
}

type ipcResult struct {
	Stdout string
	Stderr string
	Exit   int
}

func runIPCJob(ctx context.Context, runDir, script string) (ipcResult, error) {
	if err := writeShellJob(runDir, script); err != nil {
		return ipcResult{}, err
	}
	stdoutPath := filepath.Join(runDir, "stdout")
	_ = os.Remove(stdoutPath)
	if err := syscall.Mkfifo(stdoutPath, 0o600); err != nil {
		return ipcResult{}, fmt.Errorf("stdout fifo: %w", err)
	}
	_ = os.Remove(filepath.Join(runDir, "done"))
	_ = os.Remove(filepath.Join(runDir, "exit"))
	_ = os.Remove(filepath.Join(runDir, "cancel"))

	type openRes struct {
		f   *os.File
		err error
	}
	opened := make(chan openRes, 1)
	go func() {
		f, err := os.OpenFile(stdoutPath, os.O_RDONLY, 0)
		opened <- openRes{f, err}
	}()
	if err := os.WriteFile(filepath.Join(runDir, "start"), []byte("1\n"), 0o600); err != nil {
		return ipcResult{}, err
	}

	var file *os.File
	select {
	case <-ctx.Done():
		_ = os.WriteFile(filepath.Join(runDir, "cancel"), []byte("1\n"), 0o600)
		return ipcResult{}, ctx.Err()
	case res := <-opened:
		if res.err != nil {
			return ipcResult{}, res.err
		}
		file = res.f
	}
	defer file.Close()

	stop := context.AfterFunc(ctx, func() {
		_ = os.WriteFile(filepath.Join(runDir, "cancel"), []byte("1\n"), 0o600)
	})
	stdoutBytes, readErr := io.ReadAll(file)
	stop()

	deadline := time.Now().Add(15 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(runDir, "done")); err == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	exitText, _ := os.ReadFile(filepath.Join(runDir, "exit"))
	stderrText, _ := os.ReadFile(filepath.Join(runDir, "stderr"))
	code, _ := strconv.Atoi(strings.TrimSpace(string(exitText)))
	out := ipcResult{
		Stdout: string(stdoutBytes),
		Stderr: string(stderrText),
		Exit:   code,
	}
	if readErr != nil {
		return out, readErr
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, nil
}

func (s *apiServer) reapLoop() {
	if s.config.idleTimeout <= 0 {
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
		idle := now.Sub(m.meta.LastUsed) >= s.config.idleTimeout
		if idle && !m.busy && m.liveAlive() {
			m.stopLive()
		}
		m.mu.Unlock()
	}
}
