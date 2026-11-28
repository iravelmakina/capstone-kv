package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Store keeps proposals, approvals, executions and run records as JSON files:
//
//	<dir>/proposals/<id>.json   written by the agent (its container mounts only proposals/ and runs/)
//	<dir>/runs/<id>.json        the agent's record of a run: tool calls, results, proposal, check
//	<dir>/approvals/<id>.json   written by Approve, run by the author outside the container
//	<dir>/executed/<id>.json    written by the executor after it acted (or refused)
type Store struct{ Dir string }

// Approval is who approved a proposal and when.
type Approval struct {
	ID       string    `json:"id"`
	Approver string    `json:"approver"`
	At       time.Time `json:"at"`
}

// Execution is what the executor did with an approved proposal.
type Execution struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"` // "executed" or "refused: <why>"
}

var validID = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func (s Store) path(kind, id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("bad proposal id %q", id)
	}
	return filepath.Join(s.Dir, kind, id+".json"), nil
}

func (s Store) write(kind, id string, v any) error {
	p, err := s.path(kind, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s Store) read(kind, id string, v any) error {
	p, err := s.path(kind, id)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// SaveProposal stores a proposal (the agent's side). It refuses to overwrite one.
func (s Store) SaveProposal(p Proposal) error {
	if pth, err := s.path("proposals", p.ID); err == nil {
		if _, err := os.Stat(pth); err == nil {
			return fmt.Errorf("proposal %s exists", p.ID)
		}
	}
	return s.write("proposals", p.ID, p)
}

// SaveRun stores a run record.
func (s Store) SaveRun(id string, record any) error { return s.write("runs", id, record) }

// Proposal reads one.
func (s Store) Proposal(id string) (Proposal, error) {
	var p Proposal
	err := s.read("proposals", id, &p)
	return p, err
}

// ErrNotApprovable is returned for a rejected or expired proposal.
var ErrNotApprovable = errors.New("not approvable")

// Approve is the gate (ADR-0034): it re-checks the proposal against the current state,
// refuses a rejected or expired one, shows it in full and asks; only a yes writes the
// approval. confirm gets the summary and returns the person's answer.
func (s Store) Approve(id, approver string, c Context, now time.Time, confirm func(summary string) bool) error {
	p, err := s.Proposal(id)
	if err != nil {
		return err
	}
	Check(&p, c)
	if p.Rejected != "" {
		return fmt.Errorf("%w: %s", ErrNotApprovable, p.Rejected)
	}
	if p.Expired(now) {
		return fmt.Errorf("%w: made %s ago, approvals expire after %s", ErrNotApprovable, now.Sub(p.Created).Round(time.Second), Expiry)
	}
	if p.Kind == NoAction {
		return fmt.Errorf("%w: no_action has nothing to execute", ErrNotApprovable)
	}
	if !confirm(p.Summary()) {
		return errors.New("not approved")
	}
	return s.write("approvals", id, Approval{ID: id, Approver: approver, At: now.UTC()})
}

// Approved returns approved proposals the executor hasn't handled, oldest first.
func (s Store) Approved() ([]Proposal, []Approval, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "approvals", "*.json"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	var ps []Proposal
	var as []Approval
	for _, f := range files {
		id := filepath.Base(f[:len(f)-len(".json")])
		if pth, _ := s.path("executed", id); pth != "" {
			if _, err := os.Stat(pth); err == nil {
				continue
			}
		}
		var a Approval
		if err := s.read("approvals", id, &a); err != nil {
			continue
		}
		p, err := s.Proposal(id)
		if err != nil {
			continue
		}
		ps, as = append(ps, p), append(as, a)
	}
	return ps, as, nil
}

// MarkExecuted records what the executor did.
func (s Store) MarkExecuted(id, outcome string, now time.Time) error {
	return s.write("executed", id, Execution{ID: id, At: now.UTC(), Outcome: outcome})
}
