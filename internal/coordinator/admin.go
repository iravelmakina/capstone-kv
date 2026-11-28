package coordinator

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/iravelmakina/capstone-kv/internal/httpx"
)

// The admin API's cluster and migration routes (M5, ADR-0033). Reads need the status scope
// (or admin), changes the admin scope; observe checks them.

// ClusterState is GET /v1/admin/cluster: every set with its state, leader and replicas, the
// sets on the ring, and the running (or last) migration.
type ClusterState struct {
	Sets      []ShardStatus `json:"sets"`
	Ring      []string      `json:"ring"`
	Migration *Migration    `json:"migration"`
}

func (c *Coordinator) cluster(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, ClusterState{
		Sets: c.members.Status(), Ring: c.members.RingShards(), Migration: c.members.CurrentMigration(),
	})
}

// startMigration is POST /v1/admin/migrations {"add": "shard-3"} or {"remove": "shard-3"}:
// 201 with the migration; 400 for a bad body; 404 for an unknown set; 409 when another
// migration runs or the set can't join or leave now.
func (c *Coordinator) startMigration(w http.ResponseWriter, r *http.Request) {
	var req MigrationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || (req.Add != "" && !validName.MatchString(req.Add)) ||
		(req.Remove != "" && !validName.MatchString(req.Remove)) || req.TriggerRate < 0 {
		httpx.WriteError(w, http.StatusBadRequest, `body must be {"add": "<shard>"} or {"remove": "<shard>"}, optionally "triggerRate"`)
		return
	}
	mg, err := c.members.StartMigration(req, info(r).principal)
	if writeStatusError(w, err) {
		return
	}
	info(r).shard = mg.Shard
	httpx.WriteJSON(w, http.StatusCreated, mg)
}

func (c *Coordinator) currentMigration(w http.ResponseWriter, r *http.Request) {
	mg := c.members.CurrentMigration()
	if mg == nil {
		httpx.WriteError(w, http.StatusNotFound, "no migration has run")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mg)
}

// abortMigration is DELETE /v1/admin/migrations/current: 200 with the aborted migration;
// 404 when none runs; 409 once a source has cut over.
func (c *Coordinator) abortMigration(w http.ResponseWriter, r *http.Request) {
	mg, err := c.members.AbortMigration("aborted by " + info(r).principal)
	if writeStatusError(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mg)
}

// writeStatusError answers a StatusError (or any error, as 500) and reports whether it did.
func writeStatusError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		httpx.WriteError(w, se.Status, se.Msg)
	} else {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// RingShards returns the sets on the ring (before a running migration's change).
func (m *Membership) RingShards() []string { return m.ring.Shards() }
