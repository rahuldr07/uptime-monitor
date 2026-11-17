// Package mail is the system mail queue: a bounded, single-worker queue in
// front of an SMTP sender, with budgets per recipient and for the instance.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"strconv"
	"sync"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/ratelimit"
)

// Sender delivers one message; notify.Email satisfies it.
type Sender interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// Options configures a Queue.
type Options struct {
	Sender  Sender
	PerHour int // instance budget (limits.mail_per_hour)
	PerDay  int // instance daily budget (limits.mail_per_day); 0 = unlimited
	// ExemptWorkspace is not held to the alert-only share of the instance
	// budgets (the operator's workspace); it still charges the shared ones.
	ExemptWorkspace int64
	Logger          *slog.Logger     // nil = slog.Default()
	Now             func() time.Time // nil = time.Now; set as the limiters' Now
	QueueSize       int              // 0 = 100
}

type message struct {
	to, subject, body string
}

// Queue holds messages until Run delivers them.
type Queue struct {
	sender Sender
	log    *slog.Logger
	ch     chan message

	mu             sync.Mutex // serialises Send and Notice: check, compose, charge, push
	hour, day, all *ratelimit.Limiter
	allDay         *ratelimit.Limiter // nil = no daily cap
	wsHour, wsDay  *ratelimit.Limiter // fallback alerts, per workspace
	// Alert-only instance budgets at half the shared ones, so fallback alerts
	// cannot use up the mail that sign-up and password reset need.
	alertHour, alertDay *ratelimit.Limiter // alertDay is nil without a daily cap
	exempt              int64
}

// Fallback alert budgets per workspace.
const (
	alertsPerHour = 10
	alertsPerDay  = 30
)

const instanceKey = "instance"

// New returns a Queue. Call Run to start delivery.
func New(o Options) *Queue {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 100
	}
	q := &Queue{
		sender: o.Sender, log: o.Logger, ch: make(chan message, o.QueueSize),
		hour: ratelimit.New(time.Hour, 3, 0),
		day:  ratelimit.New(24*time.Hour, 10, 0),
		all:  ratelimit.New(time.Hour, o.PerHour, 1),
	}
	q.exempt = o.ExemptWorkspace
	q.alertHour = ratelimit.New(time.Hour, max(1, o.PerHour/2), 1)
	if o.PerDay > 0 {
		q.alertDay = ratelimit.New(24*time.Hour, max(1, o.PerDay/2), 1)
	}
	q.wsHour, q.wsDay = ratelimit.New(time.Hour, alertsPerHour, 0), ratelimit.New(24*time.Hour, alertsPerDay, 0)
	if o.PerDay > 0 {
		q.allDay = ratelimit.New(24*time.Hour, o.PerDay, 1)
	}
	for _, l := range []*ratelimit.Limiter{q.hour, q.day, q.all, q.allDay, q.wsHour, q.wsDay, q.alertHour, q.alertDay} {
		if l != nil {
			l.Now = o.Now
		}
	}
	return q
}

// Send checks the queue has room and the recipient budgets (3/hour, 10/day)
// and instance budget allow it, then calls compose, then records the budgets
// and queues the result without blocking. When the queue is full or a budget
// is spent, the message is dropped with a WARN (event=mail_dropped, reason,
// email_hash) and compose is NOT called, so no token is created. Send returns
// compose's error (nothing is charged then), nil otherwise. Compose runs with
// the queue locked: it must not call Send or Notice, and callers must not
// hold a store transaction across Send.
func (q *Queue) Send(to string, compose func() (subject, body string, err error)) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if reason := q.refuse(to, true); reason != "" {
		q.drop(to, reason)
		return nil
	}
	subject, body, err := compose()
	if err != nil {
		return err
	}
	q.hour.Hit(to)
	q.day.Hit(to)
	q.push(to, subject, body)
	return nil
}

// Notice queues a fixed notice (the password-change email). It is charged
// only to the instance budget, so /forgot spam cannot use up the recipient
// budget and suppress it.
func (q *Queue) Notice(to, subject, body string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if reason := q.refuse(to, false); reason != "" {
		q.drop(to, reason)
		return
	}
	q.push(to, subject, body)
}

// refuse returns why a message cannot be queued, or "" if it can. The caller
// holds mu, so the room it sees cannot be taken before push.
func (q *Queue) refuse(to string, perRecipient bool) string {
	if len(q.ch) == cap(q.ch) {
		return "queue_full"
	}
	if perRecipient {
		if ok, _ := q.hour.Allow(to); !ok {
			return "recipient_hour"
		}
		if ok, _ := q.day.Allow(to); !ok {
			return "recipient_day"
		}
	}
	return q.refuseInstance()
}

// refuseInstance checks the instance budgets; the caller holds mu.
func (q *Queue) refuseInstance() string {
	if ok, _ := q.all.Allow(instanceKey); !ok {
		return "instance"
	}
	if q.allDay != nil {
		if ok, _ := q.allDay.Allow(instanceKey); !ok {
			return "instance_day"
		}
	}
	return ""
}

// push charges the instance budget and queues; the caller holds mu.
func (q *Queue) push(to, subject, body string) {
	q.hitInstance()
	q.ch <- message{to, subject, body}
}

func (q *Queue) hitInstance() {
	q.all.Hit(instanceKey)
	if q.allDay != nil {
		q.allDay.Hit(instanceKey)
	}
}

// Admit charges one fallback alert email to the workspace budgets (10/hour,
// 30/day), the alert-only share of the instance budgets (half of each, the
// operator's workspace excepted) and the shared instance budgets, or returns
// why it was refused (and logs event=alert_mail_dropped). Each recipient is
// charged to the instance budgets. It is called once per alert, before
// delivery, so retries of the same alert are not charged again.
func (q *Queue) Admit(workspace int64, recipients int) (reason string) {
	key := strconv.FormatInt(workspace, 10)
	shared := workspace != q.exempt
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case !allowed(q.wsHour, key):
		reason = "workspace_hour"
	case !allowed(q.wsDay, key):
		reason = "workspace_day"
	case shared && !allowed(q.alertHour, instanceKey):
		reason = "alerts_hour"
	case shared && q.alertDay != nil && !allowed(q.alertDay, instanceKey):
		reason = "alerts_day"
	default:
		reason = q.refuseInstance()
	}
	if reason != "" {
		q.log.Warn("alert mail dropped", "event", "alert_mail_dropped", "workspace", workspace, "reason", reason)
		return reason
	}
	q.wsHour.Hit(key)
	q.wsDay.Hit(key)
	for range max(1, recipients) {
		if shared {
			q.alertHour.Hit(instanceKey)
			if q.alertDay != nil {
				q.alertDay.Hit(instanceKey)
			}
		}
		q.hitInstance()
	}
	return ""
}

func allowed(l *ratelimit.Limiter, key string) bool {
	ok, _ := l.Allow(key)
	return ok
}

// Deliver sends one message now, through the same sender as queued mail, and
// returns its error. Admit must have allowed it.
func (q *Queue) Deliver(ctx context.Context, to []string, subject, body string) error {
	return q.sender.Send(ctx, to, subject, body)
}

func (q *Queue) drop(to, reason string) {
	q.log.Warn("mail dropped", "event", "mail_dropped", "reason", reason, "email_hash", emailHash(to))
}

// Run delivers queued messages one at a time until ctx is cancelled. Failures
// are logged with the error class only: SMTP replies often echo the address.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-q.ch:
			if ctx.Err() != nil {
				return
			}
			if err := q.sender.Send(ctx, []string{m.to}, m.subject, m.body); err != nil {
				q.log.Warn("mail send failed", "event", "mail_failed", "error_class", errorClass(err))
			}
		}
	}
}

// errorClass names a delivery failure without any of its text, which may
// echo the recipient.
func errorClass(err error) string {
	var te *textproto.Error
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &te):
		return fmt.Sprintf("smtp_%d", te.Code)
	case errors.As(err, &ne):
		return "network"
	}
	return "other"
}

// emailHash is the first 16 hex characters of the address's SHA-256.
func emailHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])[:16]
}
