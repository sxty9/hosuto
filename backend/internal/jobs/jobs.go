// Package jobs tracks work that outlives the request which asked for it.
//
// Every other operation in hosuto finishes inside its HTTP handler, and that is the right shape for
// all of them. A migration is the exception and not by a small margin: it pulls gigabytes off a
// foreign host or unpacks an upload, then installs a server jar on top. Holding a connection open
// for that would mean a browser, a reverse proxy and a phone's radio all having to agree to wait
// several minutes, and the first one to give up would abandon a half-built server with nobody
// watching it.
//
// So the handler starts a job and returns its id, the work runs on its own context, and the UI polls.
// The registry is deliberately in-memory: a job is only interesting while it runs, and a daemon
// restart kills the work anyway — persisting the record would just preserve a status for something
// that is no longer happening.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// Job states.
const (
	StateRunning  = "running"
	StateDone     = "done"
	StateFailed   = "failed"
	StateCanceled = "canceled"
)

// retain is how long a finished job stays readable. Long enough that a UI polling every two seconds
// always sees the terminal state (and a user who switched tabs can come back to it), short enough
// that the registry cannot grow without bound.
const retain = 30 * time.Minute

// Job is a snapshot of one piece of background work, in the shape the UI renders.
//
// Done/Total are bytes or items depending on the phase, and Total is 0 while it is not yet known —
// the UI shows an indeterminate bar for that rather than a progress bar that lies about being at 0%.
type Job struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Owner    string   `json:"owner"`
	State    string   `json:"state"`
	Phase    string   `json:"phase"`
	Message  string   `json:"message,omitempty"`
	Done     int64    `json:"done"`
	Total    int64    `json:"total"`
	ServerID string   `json:"serverId,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Error    string   `json:"error,omitempty"`
	Started  int64    `json:"started"`
	Ended    int64    `json:"ended,omitempty"`
}

// Registry holds the running and recently-finished jobs.
type Registry struct {
	mu     sync.Mutex
	jobs   map[string]*entry
	nowFn  func() time.Time
	sweeps int
}

type entry struct {
	job    Job
	cancel context.CancelFunc
}

// New builds a registry.
func New() *Registry {
	return &Registry{jobs: map[string]*entry{}, nowFn: time.Now}
}

// Handle is the running job's view of itself: the only way the work reports progress. It is safe for
// concurrent use, because a transfer reports bytes from one goroutine while phases advance on another.
type Handle struct {
	r   *Registry
	id  string
	ctx context.Context
}

// Context is the job's own context, cancelled when the job is cancelled. Work must honour it —
// that is what makes an abort actually stop a multi-gigabyte transfer.
func (h *Handle) Context() context.Context { return h.ctx }

// Phase names what is happening now and resets the counters, since each phase measures its own thing.
func (h *Handle) Phase(name string) {
	h.r.update(h.id, func(j *Job) {
		j.Phase = name
		j.Done, j.Total = 0, 0
		j.Message = ""
	})
}

// Total declares how much the current phase will do, once it is known.
func (h *Handle) Total(n int64) { h.r.update(h.id, func(j *Job) { j.Total = n }) }

// Add advances the current phase's counter.
func (h *Handle) Add(n int64) { h.r.update(h.id, func(j *Job) { j.Done += n }) }

// Message sets the detail line — the file being transferred, the mod being resolved.
func (h *Handle) Message(s string) { h.r.update(h.id, func(j *Job) { j.Message = s }) }

// Note records something the operator should read once the job is done. Notes accumulate; they are
// the migration's report, not a log.
func (h *Handle) Note(s string) {
	if s == "" {
		return
	}
	h.r.update(h.id, func(j *Job) { j.Notes = append(j.Notes, s) })
}

// Notes appends several at once.
func (h *Handle) Notes(ss []string) {
	for _, s := range ss {
		h.Note(s)
	}
}

// Result records the server the job produced, as soon as it exists — so a job that fails LATER still
// tells the UI which server to open and clean up.
func (h *Handle) Result(serverID string) {
	h.r.update(h.id, func(j *Job) { j.ServerID = serverID })
}

// Start registers a job and runs fn on its own goroutine. It returns the initial snapshot, so the
// handler can answer with an id immediately.
//
// fn's returned error becomes the job's error verbatim: these strings are written for the operator
// ("the host refused these credentials"), not for a log, and rewrapping them here would blur that.
func (r *Registry) Start(kind, owner, phase string, fn func(*Handle) error) Job {
	ctx, cancel := context.WithCancel(context.Background())
	now := r.nowFn()
	j := Job{
		ID: genID(), Kind: kind, Owner: owner, State: StateRunning,
		Phase: phase, Started: now.Unix(),
	}

	r.mu.Lock()
	r.sweepLocked(now)
	r.jobs[j.ID] = &entry{job: j, cancel: cancel}
	r.mu.Unlock()

	h := &Handle{r: r, id: j.ID, ctx: ctx}
	go func() {
		defer cancel()
		err := fn(h)
		r.update(j.ID, func(j *Job) {
			j.Ended = r.nowFn().Unix()
			switch {
			case err == nil:
				j.State = StateDone
				j.Phase = "done"
			case ctx.Err() != nil:
				// A cancelled job's error is whatever the abort produced on the way out; saying
				// "canceled" is the truthful account of why it stopped.
				j.State = StateCanceled
				j.Error = "canceled"
			default:
				j.State = StateFailed
				j.Error = err.Error()
				// The registry is in-memory and a job's error reaches only the browser that was
				// watching it. Without this line a failed migration leaves NO trace anywhere on the
				// host, and the operator debugging it an hour later has nothing to read.
				log.Printf("hosuto: %s job %s failed in phase %q: %v", kind, j.ID, j.Phase, err)
			}
		})
	}()
	return j
}

// Get returns a snapshot of one job.
func (r *Registry) Get(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.jobs[id]
	if !ok {
		return Job{}, false
	}
	return e.job.clone(), true
}

// Cancel stops a running job. It reports whether there was one to stop.
func (r *Registry) Cancel(id string) bool {
	r.mu.Lock()
	e, ok := r.jobs[id]
	if !ok || e.job.State != StateRunning {
		r.mu.Unlock()
		return false
	}
	cancel := e.cancel
	r.mu.Unlock()
	cancel()
	return true
}

// ByOwner returns a user's jobs, newest first. It is what lets the UI pick a migration back up after
// a reload instead of losing track of work that is still running.
func (r *Registry) ByOwner(owner string) []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Job{}
	for _, e := range r.jobs {
		if e.job.Owner == owner {
			out = append(out, e.job.clone())
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Started > out[j-1].Started; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (r *Registry) update(id string, fn func(*Job)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.jobs[id]; ok {
		fn(&e.job)
	}
}

// sweepLocked drops finished jobs past their retention. It runs on Start rather than on a ticker: a
// registry nobody is adding to does not need tending, and a goroutine that outlives every job would
// be a leak in service of nothing.
func (r *Registry) sweepLocked(now time.Time) {
	cutoff := now.Add(-retain).Unix()
	for id, e := range r.jobs {
		if e.job.State != StateRunning && e.job.Ended > 0 && e.job.Ended < cutoff {
			delete(r.jobs, id)
		}
	}
}

func (j Job) clone() Job {
	if j.Notes != nil {
		j.Notes = append([]string(nil), j.Notes...)
	}
	return j
}

func genID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "job-" + hex.EncodeToString(b)
}
