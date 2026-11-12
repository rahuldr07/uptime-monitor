// Package mail is the system mail queue: a bounded, single-worker queue in
// front of an SMTP sender, with budgets per recipient and for the instance.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"uptimemon/internal/ratelimit"
)

// Sender delivers one message; notify.Email satisfies it.
type Sender interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// Options configures a Queue.
type Options struct {
	Sender    Sender
	PerHour   int              // instance budget (limits.mail_per_hour)
	Logger    *slog.Logger     // nil = slog.Default()
	Now       func() time.Time // nil = time.Now; set as the limiters' Now
	QueueSize int              // 0 = 100
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
}

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
	for _, l := range []*ratelimit.Limiter{q.hour, q.day, q.all} {
		l.Now = o.Now
	}
	return q
}

// Send checks the queue has room and the recipient budgets (3/hour, 10/day)
// and instance budget allow it, then calls compose, then records the budgets
// and queues the result without blocking. When the queue is full or a budget
// is spent, the message is dropped with a WARN (event=mail_dropped, reason,
// email_hash) and compose is NOT called, so no token is created. Send returns
// compose's error (nothing is charged then), nil otherwise.
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
	if ok, _ := q.all.Allow(instanceKey); !ok {
		return "instance"
	}
	return ""
}

// push charges the instance budget and queues; the caller holds mu.
func (q *Queue) push(to, subject, body string) {
	q.all.Hit(instanceKey)
	q.ch <- message{to, subject, body}
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
			if err := q.sender.Send(ctx, []string{m.to}, m.subject, m.body); err != nil {
				q.log.Warn("mail send failed", "event", "mail_failed", "error_class", fmt.Sprintf("%T", err))
			}
		}
	}
}

// emailHash is the first 16 hex characters of the address's SHA-256.
func emailHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])[:16]
}
