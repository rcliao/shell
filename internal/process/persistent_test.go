package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeCLI is an io.Pipe pair standing in for the claude subprocess: the test
// writes stdout lines the CLI would emit and reads what the bridge wrote to
// stdin.
type fakeCLI struct {
	stdoutW *io.PipeWriter // test writes CLI stdout here
	stdinR  *io.PipeReader // test reads what the proc wrote to stdin
	proc    *persistentProc
	got     chan SendResult // unsolicited results
}

func newFakeCLI(t *testing.T) *fakeCLI {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	f := &fakeCLI{stdoutW: stdoutW, stdinR: stdinR, got: make(chan SendResult, 4)}
	f.proc = newPersistentProc(stdinW, stdoutR, SessionKey{ChatID: 42}, "m", func(_ SessionKey, r SendResult) { f.got <- r })
	// Drain stdin so writes never block.
	go io.Copy(io.Discard, stdinR)
	t.Cleanup(func() { stdoutW.Close() })
	return f
}

// emit writes one CLI stdout line.
func (f *fakeCLI) emit(line string) {
	if _, err := io.WriteString(f.stdoutW, line+"\n"); err != nil {
		panic(err)
	}
}

// turn writes a complete assistant turn: one text block and a result.
func (f *fakeCLI) turn(text string) {
	f.emit(fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, text))
	f.emit(fmt.Sprintf(`{"type":"result","subtype":"success","result":%q,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}`, text))
}

func (f *fakeCLI) send(ctx context.Context) (SendResult, error) {
	return f.proc.sendMessage(ctx, AgentRequest{ChatID: 42, Text: "hi"}, nil)
}

func waitResult(t *testing.T, ch chan SendResult) SendResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for result")
	}
	return SendResult{}
}

// The init handshake must not count as a turn, and a normal exchange works.
func TestPersistentInitThenTurn(t *testing.T) {
	f := newFakeCLI(t)
	f.emit(`{"type":"system","subtype":"init","session_id":"s1"}`)
	f.emit(`{"type":"control_response","response":{"subtype":"success","request_id":"init_1"}}`)
	done := make(chan SendResult, 1)
	go func() { r, _ := f.send(context.Background()); done <- r }()
	time.Sleep(50 * time.Millisecond) // let the send get past waitIdle and write
	f.turn("hello")
	r := waitResult(t, done)
	if r.Text != "hello" || r.SessionID != "s1" {
		t.Fatalf("got %+v", r)
	}
}

// The incident shape: a whole turn arrives between messages. It must go to
// the unsolicited handler, and the next send must get ITS OWN answer.
func TestPersistentStragglerBufferedBeforeSend(t *testing.T) {
	f := newFakeCLI(t)
	f.turn("late research result")
	late := waitResult(t, f.got)
	if late.Text != "late research result" {
		t.Fatalf("unsolicited = %+v", late)
	}
	done := make(chan SendResult, 1)
	go func() { r, _ := f.send(context.Background()); done <- r }()
	time.Sleep(50 * time.Millisecond)
	f.turn("answer to the new message")
	if r := waitResult(t, done); r.Text != "answer to the new message" {
		t.Fatalf("send got %q — off by one", r.Text)
	}
}

// A CLI-initiated turn is mid-stream when the send begins: the send must
// wait for it (handler gets it) and then receive the turn that follows.
func TestPersistentStragglerMidParseAtSend(t *testing.T) {
	f := newFakeCLI(t)
	f.emit(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}`)
	time.Sleep(30 * time.Millisecond) // pump has opened the turn
	done := make(chan SendResult, 1)
	go func() { r, _ := f.send(context.Background()); done <- r }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("send returned while a CLI-initiated turn was open")
	default:
	}
	f.emit(`{"type":"result","subtype":"success","result":"partial","num_turns":1}`)
	if late := waitResult(t, f.got); !strings.Contains(late.Text, "partial") {
		t.Fatalf("unsolicited = %+v", late)
	}
	time.Sleep(30 * time.Millisecond)
	f.turn("ours")
	if r := waitResult(t, done); r.Text != "ours" {
		t.Fatalf("send got %q", r.Text)
	}
}

// EOF while a send is waiting must return (empty result), not hang.
func TestPersistentEOFWhileWaiting(t *testing.T) {
	f := newFakeCLI(t)
	done := make(chan SendResult, 1)
	go func() { r, _ := f.send(context.Background()); done <- r }()
	time.Sleep(50 * time.Millisecond)
	f.stdoutW.Close()
	r := waitResult(t, done)
	if r.Text != "" || r.Usage != nil {
		t.Fatalf("expected empty result on EOF, got %+v", r)
	}
}

// Caller gives up mid-turn: the send returns ErrTurnAbandoned, the pump stays
// alive, and the late result reaches the unsolicited handler.
func TestPersistentAbandonedTurnBecomesFollowUp(t *testing.T) {
	f := newFakeCLI(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { _, err := f.send(ctx); errCh <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTurnAbandoned) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return on ctx cancel")
	}
	f.turn("finished after the caller left")
	if late := waitResult(t, f.got); late.Text != "finished after the caller left" {
		t.Fatalf("late result = %+v", late)
	}
	select {
	case <-f.proc.pumpDone:
		t.Fatal("pump exited")
	default:
	}
}
