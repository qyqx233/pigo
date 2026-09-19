package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// turnHTTP serves the turn endpoints over real HTTP, so connections can be
// dropped the way a browser drops them. Requests carry no principal and act as
// the service token.
func turnHTTP(t *testing.T, server *apiServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions/{id}/messages", server.handleMessage)
	mux.HandleFunc("GET /api/sessions/{id}/messages", server.handleSessionMessages)
	mux.HandleFunc("GET /api/sessions/{id}/turn", server.handleAttachTurn)
	mux.HandleFunc("POST /api/sessions/{id}/turn/cancel", server.handleCancelTurn)
	mux.HandleFunc("GET /api/sessions/{id}", server.handleGetSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", server.handleDeleteSession)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// eventReader reads an NDJSON turn stream.
type eventReader struct {
	t    *testing.T
	body io.ReadCloser
	scan *bufio.Scanner
}

func openStream(t *testing.T, ctx context.Context, method, url, body string) *eventReader {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, raw)
	}
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &eventReader{t: t, body: resp.Body, scan: scan}
}

func (r *eventReader) next() (streamEvent, bool) {
	if !r.scan.Scan() {
		return streamEvent{}, false
	}
	var ev streamEvent
	if err := json.Unmarshal(r.scan.Bytes(), &ev); err != nil {
		r.t.Fatalf("bad line %q", r.scan.Text())
	}
	return ev, true
}

// until reads events until one of the given types, returning it and the text
// the stream carried (snapshot plus deltas).
func (r *eventReader) until(types ...string) (streamEvent, string) {
	var text strings.Builder
	for {
		ev, ok := r.next()
		if !ok {
			r.t.Fatalf("stream ended before %v", types)
		}
		switch ev.Type {
		case "snapshot":
			text.Reset()
			text.WriteString(ev.Text)
		case "delta":
			text.WriteString(ev.Text)
		}
		for _, want := range types {
			if ev.Type == want {
				return ev, text.String()
			}
		}
	}
}

func (r *eventReader) close() { r.body.Close() }

func postPrompt(prompt string) string {
	raw, _ := json.Marshal(messageRequest{Prompt: prompt})
	return string(raw)
}

// gatedLoop is a loopFn that emits "part1", waits for release (or for the
// turn to be stopped), then emits "part2" and finishes.
type gatedLoop struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newGatedLoop() *gatedLoop {
	return &gatedLoop{release: make(chan struct{}), started: make(chan struct{})}
}

func (g *gatedLoop) run(ctx context.Context, _ *managedSession, prompt string, emit func(streamEvent)) (string, error) {
	emit(streamEvent{Type: "delta", Text: "part1 "})
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return "part1", ctx.Err()
	}
	emit(streamEvent{Type: "delta", Text: "part2"})
	return "part1 part2:" + prompt, nil
}

// TestTurnSurvivesDisconnect: dropping the connection that started a turn does
// not stop it; reattaching shows everything so far, then the rest.
func TestTurnSurvivesDisconnect(t *testing.T) {
	server := newTestServer(t)
	loop := newGatedLoop()
	server.loopFn = loop.run
	ts := turnHTTP(t, server)
	id := createSession(t, server)

	ctx, drop := context.WithCancel(context.Background())
	stream := openStream(t, ctx, http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("go"))
	if _, text := stream.until("delta"); !strings.Contains(text, "part1") {
		t.Fatalf("first delta = %q", text)
	}
	drop()
	stream.close()

	// While it runs: another message is refused, and history does not wait
	// for the turn.
	resp, err := http.Post(ts.URL+"/api/sessions/"+id+"/messages", "application/json", strings.NewReader(postPrompt("again")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second message while running = %d, want 409", resp.StatusCode)
	}
	historyDone := make(chan int, 1)
	go func() {
		r, err := http.Get(ts.URL + "/api/sessions/" + id + "/messages")
		if err == nil {
			r.Body.Close()
			historyDone <- r.StatusCode
		}
	}()
	select {
	case code := <-historyDone:
		if code != http.StatusOK {
			t.Errorf("history = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("history blocked behind the running turn")
	}

	attached := openStream(t, context.Background(), http.MethodGet, ts.URL+"/api/sessions/"+id+"/turn", "")
	snapshot, _ := attached.until("snapshot")
	if snapshot.Text != "part1 " {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	close(loop.release)
	final, rest := attached.until("done", "error")
	attached.close()
	// The snapshot and the events after it add up to the whole reply.
	if final.Type != "done" || final.Reason != turnDone || final.Text != "part1 part2:go" || snapshot.Text+rest != "part1 part2" {
		t.Fatalf("final = %+v, text after snapshot %q", final, rest)
	}

	// The session reports how it ended, and a late attach still gets the end.
	managed, _ := server.getSession(id)
	managed.mu.Lock()
	last := managed.meta.LastTurn
	active := managed.meta.ActiveTurn
	managed.mu.Unlock()
	if active != nil || last == nil || last.Reason != turnDone {
		t.Errorf("meta active %+v last %+v", active, last)
	}
	late := openStream(t, context.Background(), http.MethodGet, ts.URL+"/api/sessions/"+id+"/turn", "")
	if ev, _ := late.until("done"); ev.Text != "part1 part2:go" {
		t.Errorf("late attach final = %+v", ev)
	}
	late.close()
}

// TestTurnCancel: the stop endpoint ends the turn, and attached clients are
// told it was stopped and what was kept.
func TestTurnCancel(t *testing.T) {
	server := newTestServer(t)
	loop := newGatedLoop()
	server.loopFn = loop.run
	ts := turnHTTP(t, server)
	id := createSession(t, server)

	stream := openStream(t, context.Background(), http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("go"))
	stream.until("delta")
	resp, err := http.Post(ts.URL+"/api/sessions/"+id+"/turn/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	final, _ := stream.until("done", "error")
	stream.close()
	if final.Type != "error" || final.Reason != turnCanceled || !strings.Contains(final.Error, "已停止") {
		t.Fatalf("final = %+v", final)
	}
	// The session is free again.
	loop2 := newGatedLoop()
	close(loop2.release)
	server.loopFn = loop2.run
	next := openStream(t, context.Background(), http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("next"))
	if ev, _ := next.until("done", "error"); ev.Type != "done" {
		t.Errorf("next turn = %+v", ev)
	}
	next.close()
}

// TestTurnStalls: a turn with no progress for the idle limit is stopped with
// the reason spelled out.
func TestTurnStalls(t *testing.T) {
	server := newTestServer(t)
	server.config.turnIdle = 100 * time.Millisecond
	server.config.turnTick = 20 * time.Millisecond
	server.loopFn = func(ctx context.Context, _ *managedSession, _ string, _ func(streamEvent)) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	ts := turnHTTP(t, server)
	id := createSession(t, server)

	resp, err := http.Post(ts.URL+"/api/sessions/"+id+"/messages", "application/json", strings.NewReader(postPrompt("hang")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	// The brake is checked every 5s by default.
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(body["error"], "没有任何进展") {
		t.Fatalf("non-streaming stalled turn = %d %v", resp.StatusCode, body)
	}
}

// TestDeleteSessionStopsTurn: deleting a session stops its turn first, and
// the turn does not write the deleted directory back.
func TestDeleteSessionStopsTurn(t *testing.T) {
	server := newTestServer(t)
	loop := newGatedLoop()
	server.loopFn = loop.run
	ts := turnHTTP(t, server)
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	root := managed.paths.Root

	stream := openStream(t, context.Background(), http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("go"))
	<-loop.started
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/sessions/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	final, _ := stream.until("done", "error")
	stream.close()
	if final.Reason != turnSessionClosed {
		t.Errorf("final = %+v", final)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("session directory came back: %v", err)
	}
}

// --- the real agent loop against a scripted endpoint -------------------------------------

// scriptLLM is an OpenAI-compatible endpoint that answers every request with a
// call to a tool that does not exist (the loop reports the error back and goes
// on), and can hold a chosen request until released.
type scriptLLM struct {
	calls    atomic.Int32
	holdAt   int32
	held     chan struct{}
	release  chan struct{}
	requests []string
	mu       sync.Mutex
}

func (f *scriptLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	n := f.calls.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, string(raw))
	f.mu.Unlock()
	if n == f.holdAt {
		close(f.held)
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	id := fmt.Sprintf("r%d", n)
	fmt.Fprintf(w, "data: {\"id\":%q,\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"step %d \"}}]}\n\n", id, n)
	fmt.Fprintf(w, "data: {\"id\":%q,\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_%d\",\"type\":\"function\",\"function\":{\"name\":\"nosuchtool\",\"arguments\":\"{}\"}}]}}]}\n\n", id, n)
	fmt.Fprintf(w, "data: {\"id\":%q,\"model\":\"m\",\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n", id)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func scriptedSession(t *testing.T, llm http.Handler) (*apiServer, string) {
	t.Helper()
	clearProviderKeys(t)
	upstream := httptest.NewServer(llm)
	t.Cleanup(upstream.Close)
	server := newTestServer(t)
	server.loopFn = nil
	if err := server.settings.putCustomProvider(customProvider{Name: "scripted", Protocol: provider.ProtocolOpenAI, BaseURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("scripted", "sk-test"); err != nil {
		t.Fatal(err)
	}
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	managed.meta.Model, managed.meta.Provider = "m", "scripted"
	return server, id
}

// TestStepLimitAndCheckpoints drives the real loop. Every answer is a tool
// call, so only the step limit ends the turn: at the limit the model is asked
// to sum up, gets one more step, and the turn stops. Meanwhile the transcript
// is saved after every step, so a restart mid-turn keeps what was done.
func TestStepLimitAndCheckpoints(t *testing.T) {
	llm := &scriptLLM{holdAt: 3, held: make(chan struct{}), release: make(chan struct{})}
	server, id := scriptedSession(t, llm)
	server.config.turnMaxSteps = 3
	ts := turnHTTP(t, server)
	managed, _ := server.getSession(id)

	stream := openStream(t, context.Background(), http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("loop forever"))

	// Mid-turn: two steps are done and the third request is held. The
	// transcript on disk already has them.
	select {
	case <-llm.held:
	case <-time.After(5 * time.Second):
		t.Fatal("third request never came")
	}
	managed.mu.Lock()
	saved, err := readTranscript(managed.paths)
	managed.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for _, e := range saved {
		if _, ok := e.Message.(agentcore.ToolResultMessage); ok {
			results++
		}
	}
	if results != 2 {
		t.Errorf("tool results saved mid-turn = %d, want 2", results)
	}
	close(llm.release)

	final, _ := stream.until("done", "error")
	stream.close()
	if final.Type != "done" || final.Reason != turnStepLimit || !strings.Contains(final.Detail, "3 步上限") {
		t.Fatalf("final = %+v", final)
	}
	// Three calls reached the limit; the fourth carried the wrap-up note and
	// was the last.
	if n := llm.calls.Load(); n != 4 {
		t.Errorf("model calls = %d, want 4", n)
	}
	llm.mu.Lock()
	lastRequest := llm.requests[len(llm.requests)-1]
	llm.mu.Unlock()
	if !strings.Contains(lastRequest, "工具调用已达 3 次上限") {
		t.Error("the wrap-up note did not reach the model")
	}
	managed.mu.Lock()
	last := managed.meta.LastTurn
	managed.mu.Unlock()
	if last == nil || last.Reason != turnStepLimit || last.Steps != 4 {
		t.Errorf("last turn = %+v", last)
	}
}

// TestHistoryHidesRunningTurn: while a turn runs, history stops at its user
// message — the attach snapshot carries the rest.
func TestHistoryHidesRunningTurn(t *testing.T) {
	llm := &scriptLLM{holdAt: 2, held: make(chan struct{}), release: make(chan struct{})}
	server, id := scriptedSession(t, llm)
	ts := turnHTTP(t, server)
	stream := openStream(t, context.Background(), http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("hello there"))
	defer stream.close()
	<-llm.held

	resp, err := http.Get(ts.URL + "/api/sessions/" + id + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	var history []historyMessage
	_ = json.NewDecoder(resp.Body).Decode(&history)
	resp.Body.Close()
	if len(history) != 1 || history[0].Content != "hello there" {
		t.Fatalf("history while running = %+v", history)
	}

	attached := openStream(t, context.Background(), http.MethodGet, ts.URL+"/api/sessions/"+id+"/turn", "")
	snapshot, _ := attached.until("snapshot")
	attached.close()
	if !strings.Contains(snapshot.Text, "step 1") || snapshot.Steps != 1 {
		t.Errorf("snapshot = %+v", snapshot)
	}
	resp, _ = http.Post(ts.URL+"/api/sessions/"+id+"/turn/cancel", "application/json", bytes.NewReader(nil))
	resp.Body.Close()
	close(llm.release)
	stream.until("done", "error")
}

// TestTurnUsageCommittedAtEnd: a turn's usage stays with the turn while it
// runs and is committed, with the session, when it ends — including when it is
// ended by a shutdown.
func TestTurnUsageCommittedAtEnd(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := map[bool]string{false: "turn finishes", true: "shutdown stops it"}[shutdown]
		t.Run(name, func(t *testing.T) {
			llm := &scriptLLM{holdAt: 3, held: make(chan struct{}), release: make(chan struct{})}
			server, id := scriptedSession(t, llm)
			server.config.turnMaxSteps = 3
			ledger, err := newLedgerStore(server.db)
			if err != nil {
				t.Fatal(err)
			}
			server.ledger = ledger
			server.meter = &meter{ledger: ledger, settings: server.settings}
			ts := turnHTTP(t, server)
			stream := openStream(t, context.Background(), http.MethodPost, ts.URL+"/api/sessions/"+id+"/messages?stream=true", postPrompt("go"))
			defer stream.close()
			<-llm.held

			// Two calls are done, and nothing of them is in the database yet.
			if n, _ := server.db.count("SELECT COUNT(*) FROM ledger WHERE session_id = ?", id); n != 0 {
				t.Fatalf("ledger rows mid-turn = %d, want 0", n)
			}
			if shutdown {
				go server.stopAllTurns(5 * time.Second)
				// The held request only ends when released or cancelled.
			}
			close(llm.release)
			stream.until("done", "error")

			// One row per call. A shutdown cancels the held third call before it
			// answers, and a call that produced nothing is not recorded; the
			// two that completed must be.
			calls := int(llm.calls.Load())
			least := calls
			if shutdown {
				least = 2
			}
			if n, _ := server.db.count("SELECT COUNT(*) FROM ledger WHERE session_id = ?", id); n < least || n > calls {
				t.Errorf("ledger rows after the turn = %d, want %d–%d", n, least, calls)
			}
			var doc string
			if err := server.db.query("SELECT meta FROM sessions WHERE id = ?", func(rows *sql.Rows) error {
				return rows.Scan(&doc)
			}, id); err != nil || !strings.Contains(doc, `"lastTurn"`) || strings.Contains(doc, `"activeTurn"`) {
				t.Errorf("session row not updated with the turn: %s %v", doc, err)
			}
		})
	}
}
