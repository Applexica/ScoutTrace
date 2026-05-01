package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/queue"
)

// Counters is a public stats snapshot.
type Counters struct {
	Attempts uint64
	Success  uint64
	Dead     uint64
}

// Options configures the dispatcher.
type Options struct {
	Queue        *queue.Queue
	Registry     *destinations.Registry
	DefaultDest  string
	BatchMax     int           // events per batch (default 25)
	PollInterval time.Duration // sleep when no work; default 250ms
	Backoff      BackoffConfig
	Logger       func(format string, args ...any)
	Now          func() time.Time
	Rand         *rand.Rand
}

// Dispatcher dequeues events and sends them via destination adapters.
type Dispatcher struct {
	opts     Options
	counters Counters
	mu       sync.Mutex
}

// New returns a Dispatcher.
func New(opts Options) *Dispatcher {
	if opts.BatchMax == 0 {
		opts.BatchMax = 25
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	if opts.Backoff.InitialMS == 0 {
		opts.Backoff = DefaultBackoff()
	}
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Dispatcher{opts: opts}
}

// RunOnce performs a single dispatch pass against every configured
// destination and returns when there's nothing further to claim. Useful
// for `scouttrace flush` and tests.
func (d *Dispatcher) RunOnce(ctx context.Context) error {
	if d.opts.Registry == nil {
		return errors.New("dispatch: nil registry")
	}
	for _, name := range d.opts.Registry.Names() {
		ad, _ := d.opts.Registry.Lookup(name)
		if err := d.runDest(ctx, ad); err != nil {
			d.opts.Logger("dispatch error dest=%s err=%v", name, err)
		}
	}
	return nil
}

// RunForever loops until ctx is cancelled.
func (d *Dispatcher) RunForever(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := d.RunOnce(ctx); err != nil {
			d.opts.Logger("dispatch RunOnce err=%v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.opts.PollInterval):
		}
	}
}

// Counters returns a counters snapshot.
func (d *Dispatcher) Counters() Counters {
	return Counters{
		Attempts: atomic.LoadUint64(&d.counters.Attempts),
		Success:  atomic.LoadUint64(&d.counters.Success),
		Dead:     atomic.LoadUint64(&d.counters.Dead),
	}
}

func (d *Dispatcher) runDest(ctx context.Context, ad destinations.Adapter) error {
	for {
		records, err := d.opts.Queue.ClaimPending(ad.Name(), d.opts.BatchMax)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		batch, err := buildBatch(records)
		if err != nil {
			return err
		}
		atomic.AddUint64(&d.counters.Attempts, 1)
		res := ad.Send(ctx, batch)
		switch {
		case res.OK:
			atomic.AddUint64(&d.counters.Success, 1)
			for _, r := range records {
				_ = d.opts.Queue.Ack(r.ID)
			}
		case res.Retriable:
			for _, r := range records {
				delay := Compute(d.opts.Backoff, r.Attempts, d.opts.Rand)
				if res.RetryAfter > delay {
					delay = res.RetryAfter
				}
				next := d.opts.Now().Add(delay)
				if r.Attempts+1 >= d.opts.Backoff.MaxRetries {
					_ = d.opts.Queue.MarkDead(r.ID, errStr(res.Err))
					atomic.AddUint64(&d.counters.Dead, 1)
				} else {
					_ = d.opts.Queue.Retry(r.ID, next, errStr(res.Err))
				}
			}
		default:
			for _, r := range records {
				_ = d.opts.Queue.MarkDead(r.ID, errStr(res.Err))
				atomic.AddUint64(&d.counters.Dead, 1)
			}
		}
	}
}

func buildBatch(records []queue.Record) (destinations.Batch, error) {
	out := destinations.Batch{
		ID:         records[0].ID, // first event id is the idempotency key
		PreparedAt: time.Now().UTC(),
	}
	for _, r := range records {
		// Pass payload bytes verbatim. Schema variants (ToolCallEvent,
		// server_crashed, queue_recovered) all flow through unchanged.
		raw := make(json.RawMessage, len(r.Payload))
		copy(raw, r.Payload)
		out.Events = append(out.Events, raw)
	}
	return out, nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
