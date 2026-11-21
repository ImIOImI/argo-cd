package multihub

// A live, local copy of the coordination Leases on each destination
// cluster: one informer per server, limited to the coordination namespace and
// the multihub label. Only cheap hints read it (C7's "may I skip this
// refresh?"); everything that decides an operation still goes to the API
// with compare-and-swap, so a stale cache costs a duplicate refresh, never a
// wrong sync.

import "time"

// watchErrorGrace: after a watch error, the cache is treated as unhealthy for
// this long, even if the informer reconnects sooner.
const watchErrorGrace = 30 * time.Second

// LeaseView is what the cache knows about one app's Lease.
type LeaseView struct {
	Refresh    RefreshRecord
	HasRefresh bool
	Pin        Pin
	HasPin     bool
}

// Cached returns the cached view of key's Lease on server. ok is false if the
// cache for server isn't started, synced or healthy, or has no such Lease.
// The first call for a server starts its watch (and returns false).
func (c *Coordinator) Cached(server string, key Key) (LeaseView, bool) {
	return LeaseView{}, false
}

// StopWatches stops every Lease watch.
func (c *Coordinator) StopWatches() {}

func (c *Coordinator) watchSynced(server string) bool  { return false }
func (c *Coordinator) watchHealthy(server string) bool { return false }
