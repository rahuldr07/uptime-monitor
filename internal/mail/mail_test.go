package mail

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

type call struct {
	to      []string
	subject string
	body    string
}

// fake records deliveries. If block is set, Send signals entered, then waits
// for block to close or ctx to end.
type fake struct {
	calls   chan call
	entered chan struct{}
	block   chan struct{}
	err     error
}

func newFake() *fake { return &fake{calls: make(chan call, 64), entered: make(chan struct{}, 64)} }

func (f *fake) Send(ctx context.Context, to []string, subject, body string) error {
	f.entered <- struct{}{}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.calls <- call{to, subject, body}
	return f.err
}

type harness struct {
	q    *Queue
	f    *fake
	now  time.Time
	logs *bytes.Buffer
}

func newHarness(perHour, size int) *harness {
	h := &harness{f: newFake(), now: time.Unix(1_700_000_000, 0), logs: &bytes.Buffer{}}
	h.q = New(Options{
		Sender: h.f, PerHour: perHour, QueueSize: size,
		Logger: slog.New(slog.NewJSONHandler(h.logs, nil)),
		Now:    func() time.Time { return h.now },
	})
	return h
}

// run starts the worker; the returned func stops it and waits for it to exit.
func (h *harness) run() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.q.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

// send returns whether compose ran.
func (h *harness) send(to string) bool {
	ran := false
	_ = h.q.Send(to, func() (string, string, error) { ran = true; return "s", "b", nil })
	return ran
}

func TestSendQueuesAndRunDelivers(t *testing.T) {
	h := newHarness(100, 0)
	if err := h.q.Send("a@example.com", func() (string, string, error) { return "Subj", "Body", nil }); err != nil {
		t.Fatal(err)
	}
	defer h.run()()
	got := <-h.f.calls
	want := call{[]string{"a@example.com"}, "Subj", "Body"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRecipientBudget(t *testing.T) {
	h := newHarness(100, 0)
	for i := 0; i < 3; i++ {
		if !h.send("a@example.com") {
			t.Fatalf("send %d should reach compose", i+1)
		}
	}
	if h.send("a@example.com") {
		t.Fatal("4th send in an hour reached compose")
	}
	h.now = h.now.Add(61 * time.Minute)
	for i := 0; i < 3; i++ { // 4th to 6th
		if !h.send("a@example.com") {
			t.Fatalf("send %d after an hour should reach compose", i+4)
		}
	}
	h.now = h.now.Add(61 * time.Minute)
	for i := 0; i < 3; i++ { // 7th to 9th
		h.send("a@example.com")
	}
	h.now = h.now.Add(61 * time.Minute)
	if !h.send("a@example.com") { // 10th
		t.Fatal("10th send in a day should reach compose")
	}
	if h.send("a@example.com") {
		t.Fatal("11th send in a day reached compose")
	}
}

func TestInstanceBudget(t *testing.T) {
	h := newHarness(2, 0)
	composed := 0
	for _, a := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		_ = h.q.Send(a, func() (string, string, error) { composed++; return "s", "b", nil })
	}
	if composed != 2 {
		t.Fatalf("composed %d, want 2", composed)
	}
	h.now = h.now.Add(time.Hour)
	for i := 0; i < 3; i++ { // the dropped recipient still has its full budget
		if ok, _ := h.q.hour.Take("c@example.com"); !ok {
			t.Fatalf("dropped message used up recipient budget (take %d)", i+1)
		}
	}
}

func TestSendDroppedSkipsCompose(t *testing.T) {
	h := newHarness(100, 0)
	for i := 0; i < 3; i++ {
		h.send("secret@example.com")
	}
	if h.send("secret@example.com") {
		t.Fatal("compose called after budget spent")
	}
	out := h.logs.String()
	if !strings.Contains(out, "mail_dropped") {
		t.Fatalf("no mail_dropped in log: %s", out)
	}
	if strings.Contains(out, "secret@example.com") {
		t.Fatalf("log contains the address: %s", out)
	}
}

func TestQueueFullDrops(t *testing.T) {
	h := newHarness(100, 1)
	h.f.block = make(chan struct{})
	stop := h.run()
	defer stop()
	defer close(h.f.block)

	h.send("a@example.com")
	<-h.f.entered // the first message is inside the sender
	if !h.send("b@example.com") {
		t.Fatal("second message should fill the slot")
	}
	if h.send("c@example.com") {
		t.Fatal("third message reached compose with a full queue")
	}
	if !strings.Contains(h.logs.String(), "mail_dropped") {
		t.Fatal("no mail_dropped logged")
	}
}

func TestComposeErrorCharged(t *testing.T) {
	h := newHarness(100, 0)
	boom := errors.New("boom")
	if err := h.q.Send("a@example.com", func() (string, string, error) { return "", "", boom }); err != boom {
		t.Fatalf("got %v, want compose's error", err)
	}
	for i := 0; i < 3; i++ {
		if !h.send("a@example.com") {
			t.Fatalf("send %d after a compose error should succeed", i+1)
		}
	}
}

func TestNoticeSkipsRecipientBudget(t *testing.T) {
	h := newHarness(100, 0)
	for i := 0; i < 3; i++ {
		h.send("a@example.com")
	}
	h.q.Notice("a@example.com", "Notice", "Body")
	defer h.run()()
	var last call
	for i := 0; i < 4; i++ {
		last = <-h.f.calls
	}
	if last.subject != "Notice" {
		t.Fatalf("notice not delivered, last = %+v", last)
	}
}

func TestRunLogsNoAddress(t *testing.T) {
	h := newHarness(100, 0)
	h.f.err = errors.New("550 rejected a@example.com")
	h.send("a@example.com")
	stop := h.run()
	<-h.f.calls
	stop() // Run logs before looping, so the line is written once it exits
	out := h.logs.String()
	if !strings.Contains(out, "mail send failed") {
		t.Fatalf("no failure logged: %s", out)
	}
	if strings.Contains(out, "a@example.com") {
		t.Fatalf("log contains the address: %s", out)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(100, 0)
	h.f.block = make(chan struct{}) // never closed: the sender only ends on ctx
	h.send("a@example.com")
	stop := h.run()
	<-h.f.entered
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
