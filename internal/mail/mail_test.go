package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/textproto"
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
	if ok, _ := h.q.day.Allow("c@example.com"); !ok {
		t.Fatal("dropped message used up the daily recipient budget")
	}
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
	h := newHarness(3, 0) // a charged error would leave less than 3 instance slots
	boom := errors.New("boom")
	if err := h.q.Send("a@example.com", func() (string, string, error) { return "", "", boom }); err != boom {
		t.Fatalf("got %v, want compose's error", err)
	}
	for i := 0; i < 3; i++ {
		if !h.send("a@example.com") {
			t.Fatalf("send %d after a compose error should succeed", i+1)
		}
	}
	if h.send("b@example.com") {
		t.Fatal("instance budget should now be spent")
	}
}

func TestNoticeSkipsRecipientBudget(t *testing.T) {
	h := newHarness(100, 0)
	h.q.Notice("a@example.com", "Notice", "Body")
	for i := 0; i < 3; i++ {
		if !h.send("a@example.com") {
			t.Fatalf("send %d after a notice should reach compose", i+1)
		}
	}
	h.q.Notice("a@example.com", "Notice2", "Body") // still delivered after 3 sends
	defer h.run()()
	var subjects []string
	for i := 0; i < 5; i++ {
		subjects = append(subjects, (<-h.f.calls).subject)
	}
	if subjects[0] != "Notice" || subjects[4] != "Notice2" {
		t.Fatalf("notices not delivered: %v", subjects)
	}
}

func TestRunLogsNoAddress(t *testing.T) {
	h := newHarness(100, 0)
	h.f.err = fmt.Errorf("send: %w", &textproto.Error{Code: 550, Msg: "rejected a@example.com"})
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
	if !strings.Contains(out, "smtp_550") {
		t.Fatalf("error class missing: %s", out)
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

func (h *harness) admit(ws int64) string { return h.q.Admit(ws, 1) }

func TestAdmitWorkspaceBudgets(t *testing.T) {
	h := newHarness(1000, 0)
	for i := 0; i < 10; i++ {
		if r := h.admit(7); r != "" {
			t.Fatalf("admit %d: %q", i+1, r)
		}
	}
	if r := h.admit(7); r != "workspace_hour" {
		t.Fatalf("11th in an hour = %q", r)
	}
	if r := h.admit(8); r != "" {
		t.Fatalf("another workspace shares the budget: %q", r)
	}
	if !strings.Contains(h.logs.String(), `"event":"alert_mail_dropped"`) || !strings.Contains(h.logs.String(), `"workspace":7`) ||
		!strings.Contains(h.logs.String(), `"reason":"workspace_hour"`) {
		t.Fatalf("no WARN: %s", h.logs)
	}
	// 30 a day: three hours of 10.
	for hr := 0; hr < 2; hr++ {
		h.now = h.now.Add(61 * time.Minute)
		for i := 0; i < 10; i++ {
			if r := h.admit(7); r != "" {
				t.Fatalf("hour %d admit %d: %q", hr+2, i+1, r)
			}
		}
	}
	h.now = h.now.Add(61 * time.Minute)
	if r := h.admit(7); r != "workspace_day" {
		t.Fatalf("31st in a day = %q", r)
	}
	h.now = h.now.Add(24 * time.Hour)
	if r := h.admit(7); r != "" {
		t.Fatalf("a day later = %q", r)
	}
}

func TestAdmitCountsAgainstInstanceBudgets(t *testing.T) {
	h := newHarness(4, 0) // alerts get half: 2 an hour
	if h.admit(2) != "" || h.admit(3) != "" {
		t.Fatal("two alerts fit the alert share")
	}
	if r := h.admit(4); r != "alerts_hour" {
		t.Fatalf("3rd alert = %q", r)
	}
	// The alerts used half the instance budget; system mail gets the rest.
	if !h.send("a@example.com") || !h.send("b@example.com") {
		t.Fatal("system mail starved by alerts")
	}
	if h.send("c@example.com") {
		t.Fatal("system mail got through a spent instance budget")
	}
}

func TestAlertShareLeavesRoomForVerificationMail(t *testing.T) {
	h := newHarness(10, 0)
	for ws := int64(2); ; ws++ { // many workspaces, each within its own budget
		if h.admit(ws) != "" {
			break
		}
		if ws > 50 {
			t.Fatal("alerts were never limited")
		}
	}
	for i := 0; i < 5; i++ {
		if !h.send(fmt.Sprintf("u%d@example.com", i)) {
			t.Fatalf("verification %d starved by alerts", i+1)
		}
	}
}

func TestOperatorWorkspaceExemptFromAlertShare(t *testing.T) {
	q := New(Options{Sender: newFake(), PerHour: 4, ExemptWorkspace: 1, Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))})
	if q.Admit(2, 1) != "" || q.Admit(3, 1) != "" || q.Admit(4, 1) != "alerts_hour" {
		t.Fatal("tenants share half the budget")
	}
	if q.Admit(1, 1) != "" || q.Admit(1, 1) != "" {
		t.Fatal("operator should not be held to the alert share")
	}
	if r := q.Admit(1, 1); r != "instance" {
		t.Fatalf("operator still charges the shared budget: %q", r)
	}
}

func TestAdmitChargesEachRecipient(t *testing.T) {
	h := newHarness(8, 0) // alert share 4
	if h.admit(2) != "" {
		t.Fatal("first")
	}
	if r := h.q.Admit(3, 3); r != "" { // 1 + 3 = 4 of the share
		t.Fatal(r)
	}
	if r := h.admit(4); r != "alerts_hour" {
		t.Fatalf("recipients were not charged: %q", r)
	}
}

func TestMailPerDayCapsAllSystemMail(t *testing.T) {
	f := newFake()
	now := time.Unix(1_700_000_000, 0)
	q := New(Options{Sender: f, PerHour: 100, PerDay: 4, Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		Now: func() time.Time { return now }})
	compose := func() (string, string, error) { return "s", "b", nil }
	_ = q.Send("a@example.com", compose)            // 1
	q.Notice("b@example.com", "s", "b")             // 2
	if q.Admit(2, 1) != "" || q.Admit(3, 1) != "" { // 3, 4: the alert share is 2 a day
		t.Fatal("two alerts should fit")
	}
	if r := q.Admit(4, 1); r != "alerts_day" {
		t.Fatalf("5th = %q", r)
	}
	ran := false
	_ = q.Send("c@example.com", func() (string, string, error) { ran = true; return "s", "b", nil })
	if ran {
		t.Fatal("queued mail got through the daily cap")
	}
	now = now.Add(25 * time.Hour)
	if q.Admit(2, 1) != "" {
		t.Fatal("cap should lapse after a day")
	}
}

func TestPerDayZeroIsUnlimited(t *testing.T) {
	h := newHarness(1000, 0)
	for ws := int64(1); ws <= 200; ws++ {
		if r := h.admit(ws); r != "" {
			t.Fatalf("ws %d: %q", ws, r)
		}
	}
}
