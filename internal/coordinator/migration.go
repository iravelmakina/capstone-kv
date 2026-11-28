package coordinator

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
)

// Migrations: the coordinator's record of a ring change and how far it got (ADR-0033, spec
// shard-rebalancing). One runs at a time. This file holds the record, its persistence and
// the routing it implies; moving the keys is the runner's job.

// Migration phases.
const (
	PhaseCopying = "copying" // targets copy each source's keys, then follow its log
	PhaseCutover = "cutover" // at least one source has switched; can only finish now
	PhasePurging = "purging" // every source switched; sources drop the moved keys
	PhaseDone    = "done"
	PhaseAborted = "aborted"
)

// Kinds of ring change.
const (
	KindAdd    = "add"    // a pending set joins the ring
	KindRemove = "remove" // a set leaves the ring
)

// migrationKey is where the record lives in the coordinator's leader store; "~" can't be
// in a shard ID (validName), so it never collides with a set.
const migrationKey = "~migration"

// Migration is one ring change. Before and After are the sets on the ring before and after
// it; a key moves if its owner differs between the two rings. Sources are the sets keys
// move away from: every set on the ring for an add, the leaving set for a remove.
type Migration struct {
	ID          string                     `json:"id"`
	Kind        string                     `json:"kind"`
	Shard       string                     `json:"shard"`
	Before      []string                   `json:"before"`
	After       []string                   `json:"after"`
	VNodes      int                        `json:"vnodes"`
	Phase       string                     `json:"phase"`
	Sources     map[string]*SourceProgress `json:"sources"`
	Started     time.Time                  `json:"started"`
	Finished    time.Time                  `json:"finished,omitzero"`
	RequestedBy string                     `json:"requestedBy"`
	// TriggerRate is the request rate (per second) that made the autoscaler start this
	// migration, kept for its scale-in rule (ADR-0032); 0 when a person started it.
	TriggerRate float64 `json:"triggerRate,omitempty"`
	Reason      string  `json:"reason,omitempty"`    // why it was aborted
	CleanedUp   bool    `json:"cleanedUp,omitempty"` // aborted: targets purged, sources unfrozen
}

// SourceProgress is how far one source is.
type SourceProgress struct {
	CommitPoint  uint64 `json:"commitPoint"`  // the source's commit point the copy started from
	Copied       int64  `json:"copied"`       // records applied on targets so far
	Left         int64  `json:"left"`         // records the source still has to send (estimate)
	TailLSN      uint64 `json:"tailLSN"`      // the source LSN the targets have applied up to
	SnapshotDone bool   `json:"snapshotDone"` // its state as of CommitPoint is on the targets
	CutOver      bool   `json:"cutOver"`      // its moving keys now belong to their new owners
	Purged       bool   `json:"purged"`       // it dropped the moved keys
	Unfrozen     bool   `json:"unfrozen"`     // its freeze is lifted, so a later change can move keys back
}

// Running reports whether the migration still changes routing.
func (mg *Migration) Running() bool {
	return mg != nil && mg.Phase != PhaseDone && mg.Phase != PhaseAborted
}

// MigrationRequest is the body of POST /v1/admin/migrations: exactly one of Add, Remove.
type MigrationRequest struct {
	Add         string  `json:"add,omitempty"`
	Remove      string  `json:"remove,omitempty"`
	TriggerRate float64 `json:"triggerRate,omitempty"`
}

// StartMigration checks a ring change and records it (persisted before it takes effect).
// An add needs a pending set with a leader and a quorum of live replicas; a remove needs a
// set on the ring that isn't a base set. No other migration may be running.
func (m *Membership) StartMigration(req MigrationRequest, by string) (*Migration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mig.Running() {
		return nil, &StatusError{http.StatusConflict, "migration " + m.mig.ID + " is running"}
	}
	kind, id := KindAdd, req.Add
	if req.Remove != "" {
		kind, id = KindRemove, req.Remove
	}
	if (req.Add == "") == (req.Remove == "") {
		return nil, &StatusError{http.StatusBadRequest, `body must name one set: {"add": "<shard>"} or {"remove": "<shard>"}`}
	}
	set := m.shards[id]
	if set == nil {
		return nil, &StatusError{http.StatusNotFound, "shard " + id + " not found"}
	}
	before := m.ring.Shards()
	var after []string
	switch kind {
	case KindAdd:
		if set.state != StatePending {
			return nil, &StatusError{http.StatusConflict, id + " is " + set.state + ", not pending"}
		}
		if err := m.servingLocked(id, set); err != nil {
			return nil, err
		}
		after = append(slices.Clone(before), id)
		sort.Strings(after)
	case KindRemove:
		if set.base {
			return nil, &StatusError{http.StatusConflict, id + " is a base set: only sets added later can be removed"}
		}
		if set.state != StateActive {
			return nil, &StatusError{http.StatusConflict, id + " is " + set.state + ", not active"}
		}
		after = slices.DeleteFunc(slices.Clone(before), func(s string) bool { return s == id })
	}

	sources := map[string]*SourceProgress{}
	if kind == KindAdd {
		for _, s := range before {
			sources[s] = &SourceProgress{}
		}
	} else {
		sources[id] = &SourceProgress{}
	}
	now := m.now().UTC()
	mg := &Migration{ID: fmt.Sprintf("%s-%s-%d", kind, id, now.Unix()), Kind: kind, Shard: id,
		Before: before, After: after, VNodes: m.ring.VirtualNodes(), Phase: PhaseCopying,
		Sources: sources, Started: now, RequestedBy: by, TriggerRate: req.TriggerRate}

	prevState := set.state
	if kind == KindRemove {
		set.state = StateLeaving
	}
	if err := m.saveMigrationLocked(mg); err != nil {
		set.state = prevState
		return nil, err
	}
	if err := m.saveLocked(id, set, set.leader, set.epoch); err != nil {
		set.state = prevState
		return nil, err
	}
	m.mig, m.after = mg, ring.Of(mg.VNodes, after)
	log.Printf("migration %s started: %v -> %v", mg.ID, before, after)
	return mg.clone(), nil
}

// servingLocked checks that a set can take traffic: a confirmed leader and a quorum of
// live replicas. Callers hold m.mu.
func (m *Membership) servingLocked(id string, set *replicaSet) error {
	if info := set.replicas[set.leader]; set.leader == "" || info == nil || info.epoch < set.epoch || !m.liveLocked(info) {
		return &StatusError{http.StatusConflict, id + " has no confirmed live leader yet"}
	}
	live := 0
	for _, info := range set.replicas {
		if m.liveLocked(info) {
			live++
		}
	}
	if q := m.cfg.Replicas/2 + 1; live < q {
		return &StatusError{http.StatusConflict, fmt.Sprintf("%s has %d live replicas, %d needed", id, live, q)}
	}
	return nil
}

// AbortMigration stops the running migration, which is possible only before any source
// has cut over: the old ring stays, a leaving set becomes active again. The runner then
// purges what targets copied.
func (m *Membership) AbortMigration(reason string) (*Migration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg := m.mig
	if !mg.Running() {
		return nil, &StatusError{http.StatusNotFound, "no migration is running"}
	}
	for src, p := range mg.Sources {
		if p.CutOver {
			return nil, &StatusError{http.StatusConflict, "source " + src + " has cut over: the migration can only finish"}
		}
	}
	next := mg.clone()
	next.Phase, next.Finished, next.Reason = PhaseAborted, m.now().UTC(), reason
	if err := m.saveMigrationLocked(next); err != nil {
		return nil, err
	}
	if set := m.shards[mg.Shard]; set != nil && mg.Kind == KindRemove && set.state == StateLeaving {
		set.state = StateActive
		if err := m.saveLocked(mg.Shard, set, set.leader, set.epoch); err != nil {
			log.Printf("migration %s aborted, but %s's state not saved: %v", mg.ID, mg.Shard, err)
		}
	}
	m.mig, m.after = next, nil
	log.Printf("migration %s aborted: %s", mg.ID, reason)
	return next.clone(), nil
}

// CurrentMigration returns the running migration, or else the last one, or nil.
func (m *Membership) CurrentMigration() *Migration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mig.clone()
}

// ownerLocked is the set that serves key now. With no migration running, its owner on the
// ring. During one, a key whose owner differs between the "before" and "after" rings stays
// with its source until that source cuts over (ADR-0033), then goes to its new owner.
// Callers hold m.mu (read is enough).
func (m *Membership) ownerLocked(key []byte) (string, bool) {
	id, found := m.ring.Lookup(key)
	if !found || !m.mig.Running() || m.after == nil {
		return id, found
	}
	to, ok := m.after.Lookup(key)
	if !ok || to == id {
		return id, true
	}
	if p := m.mig.Sources[id]; p != nil && p.CutOver {
		return to, true
	}
	return id, true
}

func (m *Membership) saveMigrationLocked(mg *Migration) error {
	raw, err := json.Marshal(mg)
	if err != nil {
		return err
	}
	if err := m.cfg.Leaders.Put(migrationKey, raw); err != nil {
		return fmt.Errorf("save migration %s: %w", mg.ID, err)
	}
	return nil
}

// loadMigrationLocked reads the record at start; a running migration rebuilds its "after"
// ring, so routing continues where it was.
func (m *Membership) loadMigrationLocked() error {
	raw, ok := m.cfg.Leaders.Get(migrationKey)
	if !ok {
		return nil
	}
	var mg Migration
	if err := json.Unmarshal(raw, &mg); err != nil {
		return fmt.Errorf("migration record: %w", err)
	}
	m.mig = &mg
	if mg.Running() {
		m.after = ring.Of(mg.VNodes, mg.After)
		log.Printf("migration %s resumes in phase %s", mg.ID, mg.Phase)
	}
	return nil
}

// clone is a deep copy, so callers outside the lock can't race the runner.
func (mg *Migration) clone() *Migration {
	if mg == nil {
		return nil
	}
	c := *mg
	c.Before, c.After = slices.Clone(mg.Before), slices.Clone(mg.After)
	c.Sources = make(map[string]*SourceProgress, len(mg.Sources))
	for k, v := range mg.Sources {
		p := *v
		c.Sources[k] = &p
	}
	return &c
}
