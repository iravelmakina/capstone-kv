package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// The migration runner (ADR-0033): one goroutine in the coordinator that moves a running
// migration forward, one step at a time, persisting progress after each, so a restart
// resumes where it was. Per source set (one at a time):
//
//  1. snapshot: the source's commit point C, then its moving keys as of C, page by page,
//     applied on their new owners (throttled);
//  2. tail: the source's committed records after C whose keys move, applied the same way,
//     until few are left;
//  3. cutover: hold new writes to the source's moving keys, have the source freeze them
//     (a replicated record at LSN F, after which it refuses their writes with 409),
//     follow the tail up to F, switch the keys to their new owners (persisted), release
//     the held writes; if F isn't reached within CutoverWait, unfreeze and retry later.
//
// Then: purge the moved keys on every source, lift the freezes (so a later change can
// move keys back), make the "after" ring the ring and the joining set active (or the
// leaving one retired). An aborted migration is cleaned up: targets drop what they got,
// sources are unfrozen.

// RunnerConfig tunes the runner. Zero values take the defaults.
type RunnerConfig struct {
	Rate        int           // records copied per second, at most (ADR-0033: throttled); 2,000
	Page        int           // keys per snapshot page and records per tail read; 1,000
	Tick        time.Duration // pause when there is nothing to do; 200 ms
	LagSmall    uint64        // tail records left before a cutover is tried; 200
	CutoverWait time.Duration // the cutover's write hold; 2 s (ADR-0033)
	Backoff     time.Duration // pause after a failed step; 1 s

	hooks *runnerHooks // tests: called at points inside a cutover
}

type runnerHooks struct {
	afterHold   func(source string) // the hold is set, the freeze not yet asked for
	afterFreeze func(source string) // the freeze has committed, the switch not yet made
}

func (c RunnerConfig) withDefaults() RunnerConfig {
	if c.Rate <= 0 {
		c.Rate = 2000
	}
	if c.Page <= 0 {
		c.Page = 1000
	}
	if c.Tick <= 0 {
		c.Tick = 200 * time.Millisecond
	}
	if c.LagSmall == 0 {
		c.LagSmall = 200
	}
	if c.CutoverWait <= 0 {
		c.CutoverWait = 2 * time.Second
	}
	if c.Backoff <= 0 {
		c.Backoff = time.Second
	}
	return c
}

// errCutoverMissed: the targets didn't reach the freeze record in time; retried later.
var errCutoverMissed = errors.New("cutover missed its deadline; unfrozen, will retry")

// RunMigrations moves migrations forward until ctx ends.
func (m *Membership) RunMigrations(ctx context.Context, cfg RunnerConfig) {
	cfg = cfg.withDefaults()
	for ctx.Err() == nil {
		mg := m.CurrentMigration()
		var err error
		idle := false
		switch {
		case mg == nil, mg.Phase == PhaseDone, mg.Phase == PhaseAborted && mg.CleanedUp:
			idle = true
		case mg.Phase == PhaseAborted:
			err = m.cleanUp(ctx, mg)
		default:
			idle, err = m.step(ctx, cfg, mg)
		}
		wait := time.Duration(0)
		switch {
		case err != nil:
			log.Printf("migration %s: %v", mg.ID, err)
			wait = cfg.Backoff
		case idle:
			wait = cfg.Tick
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
}

// step does one unit of work and reports whether there was nothing to do.
func (m *Membership) step(ctx context.Context, cfg RunnerConfig, mg *Migration) (bool, error) {
	change := shard.RingChange{ID: mg.ID, Before: mg.Before, After: mg.After, VNodes: mg.VNodes}
	sources := make([]string, 0, len(mg.Sources))
	for s := range mg.Sources {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	for _, src := range sources {
		p := mg.Sources[src]
		switch {
		case p.CutOver:
			continue
		case !p.SnapshotDone:
			return false, m.snapshot(ctx, cfg, mg, change, src)
		default:
			return m.tailOrCutover(ctx, cfg, mg, change, src, p)
		}
	}
	if mg.Phase != PhasePurging {
		return false, m.update(mg.ID, func(mg *Migration) { mg.Phase = PhasePurging })
	}
	for _, src := range sources {
		if !mg.Sources[src].Purged {
			if _, err := m.callLeader(ctx, src, "purge", shard.MigrateRequest{Change: change, Which: "away"}); err != nil {
				return false, fmt.Errorf("purge on %s: %w", src, err)
			}
			m.migProgress.Add(1)
			return false, m.update(mg.ID, func(mg *Migration) { mg.Sources[src].Purged = true })
		}
	}
	for _, src := range sources {
		if !mg.Sources[src].Unfrozen {
			if _, err := m.callLeader(ctx, src, "freeze", shard.MigrateRequest{Change: change, Unfreeze: true}); err != nil {
				return false, fmt.Errorf("unfreeze on %s: %w", src, err)
			}
			return false, m.update(mg.ID, func(mg *Migration) { mg.Sources[src].Unfrozen = true })
		}
	}
	return false, m.finish(mg.ID)
}

// snapshot copies a source's moving keys as of its commit point.
func (m *Membership) snapshot(ctx context.Context, cfg RunnerConfig, mg *Migration, change shard.RingChange, src string) error {
	cp, err := m.callLeader(ctx, src, "commit", shard.MigrateRequest{Change: change})
	if err != nil {
		return fmt.Errorf("commit point of %s: %w", src, err)
	}
	after := ""
	for {
		page, err := m.callLeader(ctx, src, "state", shard.MigrateRequest{Change: change, Upto: cp.Commit, After: after, Limit: cfg.Page})
		if err != nil {
			return fmt.Errorf("state of %s: %w", src, err)
		}
		if err := m.applyCopies(ctx, cfg, mg.ID, change, src, page.Copies); err != nil {
			return err
		}
		if !page.More || len(page.Copies) == 0 {
			break
		}
		after = page.Copies[len(page.Copies)-1].Key
	}
	return m.update(mg.ID, func(mg *Migration) {
		p := mg.Sources[src]
		p.CommitPoint, p.TailLSN, p.SnapshotDone = cp.Commit, cp.Commit, true
	})
}

// tailOrCutover follows a source's committed tail; once few records are left, it cuts over.
func (m *Membership) tailOrCutover(ctx context.Context, cfg RunnerConfig, mg *Migration, change shard.RingChange, src string, p *SourceProgress) (bool, error) {
	next, err := m.tailOnce(ctx, cfg, mg.ID, change, src, p.TailLSN)
	if err != nil {
		return false, err
	}
	cp, err := m.callLeader(ctx, src, "commit", shard.MigrateRequest{Change: change})
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	if live := m.mig; live != nil && live.ID == mg.ID {
		live.Sources[src].Left = int64(cp.Commit) - int64(next)
	}
	m.mu.Unlock()
	if cp.Commit > next+cfg.LagSmall {
		return next == p.TailLSN, nil // more to follow; idle only if nothing moved
	}
	return false, m.cutover(ctx, cfg, mg, change, src)
}

// tailOnce applies one page of a source's tail from LSN from and returns how far it got.
func (m *Membership) tailOnce(ctx context.Context, cfg RunnerConfig, id string, change shard.RingChange, src string, from uint64) (uint64, error) {
	t, err := m.callLeader(ctx, src, "tail", shard.MigrateRequest{Change: change, From: from, Limit: cfg.Page})
	if err != nil {
		return from, fmt.Errorf("tail of %s: %w", src, err)
	}
	if err := m.applyCopies(ctx, cfg, id, change, src, t.Copies); err != nil {
		return from, err
	}
	if t.Next > from {
		m.migProgress.Add(t.Next - from)
		if err := m.update(id, func(mg *Migration) { mg.Sources[src].TailLSN = t.Next }); err != nil {
			return from, err
		}
	}
	return max(t.Next, from), nil
}

// cutover switches a source's moving keys to their new owners (ADR-0033, step 5).
func (m *Membership) cutover(ctx context.Context, cfg RunnerConfig, mg *Migration, change shard.RingChange, src string) error {
	start := time.Now()
	hold := m.setCutoverHold(src)
	defer m.releaseCutoverHold(src, hold)
	if h := cfg.hooks; h != nil && h.afterHold != nil {
		h.afterHold(src)
	}
	fr, err := m.callLeader(ctx, src, "freeze", shard.MigrateRequest{Change: change})
	if err != nil {
		return fmt.Errorf("freeze on %s: %w", src, err)
	}
	if h := cfg.hooks; h != nil && h.afterFreeze != nil {
		h.afterFreeze(src)
	}
	unfreeze := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := m.callLeader(ctx, src, "freeze", shard.MigrateRequest{Change: change, Unfreeze: true}); err != nil {
			log.Printf("migration %s: unfreeze on %s: %v (retried when the cutover is retried)", mg.ID, src, err)
		}
	}
	tail := mg.Sources[src].TailLSN
	for tail < fr.LSN {
		if time.Since(start) > cfg.CutoverWait {
			unfreeze()
			return errCutoverMissed
		}
		next, err := m.tailOnce(ctx, cfg, mg.ID, change, src, tail)
		if err != nil {
			unfreeze()
			return err
		}
		tail = next
	}
	// The targets hold every write the source accepted for these keys: switch them.
	m.mu.Lock()
	live := m.mig
	if live == nil || live.ID != mg.ID || !live.Running() {
		m.mu.Unlock()
		unfreeze() // aborted meanwhile: keep the old owner
		return nil
	}
	next := live.clone()
	next.Sources[src].CutOver = true
	next.Phase = PhaseCutover
	if err := m.saveMigrationLocked(next); err != nil {
		m.mu.Unlock()
		unfreeze()
		return err
	}
	m.mig = next
	m.mu.Unlock()
	m.migProgress.Add(1)
	log.Printf("migration %s: %s cut over (writes held %s)", mg.ID, src, time.Since(start).Round(time.Millisecond))
	return nil
}

// applyCopies sends copies to the leaders of their new owners, throttled to cfg.Rate.
func (m *Membership) applyCopies(ctx context.Context, cfg RunnerConfig, id string, change shard.RingChange, src string, copies []store.Copy) error {
	if len(copies) == 0 {
		return nil
	}
	start := time.Now()
	byTarget := map[string][]store.Copy{}
	for _, c := range copies {
		_, to := change.Owners(c.Key)
		byTarget[to] = append(byTarget[to], c)
	}
	applied := 0
	for target, cs := range byTarget {
		ans, err := m.callLeader(ctx, target, "apply", shard.MigrateRequest{Change: change, Copies: cs})
		if err != nil {
			return fmt.Errorf("apply on %s: %w", target, err)
		}
		applied += ans.Applied
	}
	m.migProgress.Add(uint64(len(copies)))
	if err := m.update(id, func(mg *Migration) { mg.Sources[src].Copied += int64(applied) }); err != nil {
		return err
	}
	if pause := time.Duration(len(copies))*time.Second/time.Duration(cfg.Rate) - time.Since(start); pause > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(pause):
		}
	}
	return nil
}

// finish makes the change the ring (ADR-0033, step 7).
func (m *Membership) finish(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg := m.mig
	if mg == nil || mg.ID != id || !mg.Running() {
		return nil
	}
	set := m.shards[mg.Shard]
	next := mg.clone()
	next.Phase, next.Finished = PhaseDone, m.now().UTC()
	if err := m.saveMigrationLocked(next); err != nil {
		return err
	}
	if set != nil {
		if mg.Kind == KindAdd {
			set.state = StateActive
			m.ring.Add(mg.Shard)
		} else {
			set.state = StateRetired
			m.ring.Remove(mg.Shard)
		}
		if err := m.saveLocked(mg.Shard, set, set.leader, set.epoch); err != nil {
			log.Printf("migration %s done, but %s's state not saved: %v", id, mg.Shard, err)
		}
	}
	m.mig, m.after = next, nil
	log.Printf("migration %s done: ring %v", id, m.ring.Shards())
	return nil
}

// cleanUp undoes an aborted migration's copies and freezes.
func (m *Membership) cleanUp(ctx context.Context, mg *Migration) error {
	change := shard.RingChange{ID: mg.ID, Before: mg.Before, After: mg.After, VNodes: mg.VNodes}
	targets := []string{mg.Shard}
	if mg.Kind == KindRemove {
		targets = mg.After
	}
	for _, t := range targets {
		if _, err := m.callLeader(ctx, t, "purge", shard.MigrateRequest{Change: change, Which: "in"}); err != nil {
			return fmt.Errorf("clean-up purge on %s: %w", t, err)
		}
	}
	for src := range mg.Sources {
		if _, err := m.callLeader(ctx, src, "freeze", shard.MigrateRequest{Change: change, Unfreeze: true}); err != nil {
			return fmt.Errorf("clean-up unfreeze on %s: %w", src, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mig == nil || m.mig.ID != mg.ID {
		return nil
	}
	next := m.mig.clone()
	next.CleanedUp = true
	if err := m.saveMigrationLocked(next); err != nil {
		return err
	}
	m.mig = next
	return nil
}

// update changes the running migration's record (if it is still id) and persists it.
func (m *Membership) update(id string, change func(*Migration)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mig == nil || m.mig.ID != id {
		return nil
	}
	next := m.mig.clone()
	change(next)
	if err := m.saveMigrationLocked(next); err != nil {
		return err
	}
	m.mig = next
	return nil
}

// setCutoverHold starts holding writes to src's moving keys (WaitCutover).
func (m *Membership) setCutoverHold(src string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cutoverHolds == nil {
		m.cutoverHolds = map[string]chan struct{}{}
	}
	h := make(chan struct{})
	m.cutoverHolds[src] = h
	return h
}

func (m *Membership) releaseCutoverHold(src string, h chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cutoverHolds[src] == h {
		delete(m.cutoverHolds, src)
	}
	close(h)
}

// WaitCutover waits while key's source is in a cutover and key moves (only those writes
// are held, ADR-0033), at most the cutover's budget plus a margin, and returns how long.
func (m *Membership) WaitCutover(ctx context.Context, routingKey []byte) (string, time.Duration) {
	m.mu.RLock()
	var hold chan struct{}
	src, _ := m.ring.Lookup(routingKey)
	if m.mig.Running() && m.after != nil {
		if to, _ := m.after.Lookup(routingKey); to != src {
			hold = m.cutoverHolds[src]
		}
	}
	m.mu.RUnlock()
	if hold == nil {
		return src, 0
	}
	start := time.Now()
	t := time.NewTimer(TransferBudget + time.Second)
	defer t.Stop()
	select {
	case <-hold:
	case <-t.C:
	case <-ctx.Done():
	}
	return src, time.Since(start)
}

// callLeader sends a migrate request to a set's leader (looked up now) and decodes the
// answer; a non-200 is an error with the replica's message.
func (m *Membership) callLeader(ctx context.Context, setID, endpoint string, req shard.MigrateRequest) (shard.MigrateAnswer, error) {
	addr, ok := m.leaderAddr(setID)
	if !ok {
		return shard.MigrateAnswer{}, fmt.Errorf("%s has no live leader", setID)
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/internal/v1/migrate/"+endpoint, bytes.NewReader(body))
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := m.ctl.Do(hreq)
	if err != nil {
		return shard.MigrateAnswer{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return shard.MigrateAnswer{}, fmt.Errorf("%s %s: %d %s", setID, endpoint, resp.StatusCode, errorText(raw))
	}
	var ans shard.MigrateAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return ans, fmt.Errorf("%s %s: %w", setID, endpoint, err)
	}
	return ans, nil
}

// leaderAddr is a set's live leader's address.
func (m *Membership) leaderAddr(setID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := m.shards[setID]
	if set == nil || set.leader == "" {
		return "", false
	}
	info := set.replicas[set.leader]
	if !m.liveLocked(info) {
		return "", false
	}
	return info.address, true
}
