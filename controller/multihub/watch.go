package multihub

// A live, local copy of the coordination Leases on each destination
// cluster: one informer per server, limited to the coordination namespace and
// the multihub label. Only cheap hints read it (C7's "may I skip this
// refresh?"); everything that decides an operation still goes to the API
// with compare-and-swap, so a stale cache costs a duplicate refresh, never a
// wrong sync.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

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

type leaseWatch struct {
	informer cache.SharedIndexInformer
	stop     chan struct{}
	lastErr  atomic.Int64 // unix nanos of the last watch error (0 = none)
}

type leaseWatches struct {
	mu sync.Mutex
	m  map[string]*leaseWatch
}

// Cached returns the cached view of key's Lease on server. ok is false if the
// cache for server isn't started, synced or healthy, or has no such Lease.
// The first call for a server starts its watch (and returns false).
func (c *Coordinator) Cached(server string, key Key) (LeaseView, bool) {
	if c == nil {
		return LeaseView{}, false
	}
	w := c.watch(server)
	if w == nil || !c.healthy(w) {
		return LeaseView{}, false
	}
	obj, exists, err := w.informer.GetStore().GetByKey(c.cfg.Namespace + "/" + key.LeaseName())
	if err != nil || !exists {
		return LeaseView{}, false
	}
	lease, ok := obj.(*coordinationv1.Lease)
	if !ok {
		return LeaseView{}, false
	}
	var v LeaseView
	v.Refresh, v.HasRefresh = RefreshRecordOf(lease)
	v.Pin, v.HasPin = pinOf(lease)
	return v, true
}

// StopWatches stops every Lease watch.
func (c *Coordinator) StopWatches() {
	if c == nil {
		return
	}
	c.watches.mu.Lock()
	defer c.watches.mu.Unlock()
	for server, w := range c.watches.m {
		close(w.stop)
		delete(c.watches.m, server)
	}
}

// watch returns server's watch, starting it if needed. It returns nil when
// it has just been started or can't be.
func (c *Coordinator) watch(server string) *leaseWatch {
	c.watches.mu.Lock()
	defer c.watches.mu.Unlock()
	if w, ok := c.watches.m[server]; ok {
		return w
	}
	client, err := c.clients(server)
	if err != nil {
		return nil
	}
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithNamespace(c.cfg.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = LabelManagedBy + "=" + LabelManagedByValue
		}))
	w := &leaseWatch{informer: factory.Coordination().V1().Leases().Informer(), stop: make(chan struct{})}
	_ = w.informer.SetWatchErrorHandlerWithContext(func(_ context.Context, _ *cache.Reflector, _ error) {
		w.lastErr.Store(c.now().UnixNano())
	})
	go w.informer.Run(w.stop)
	if c.watches.m == nil {
		c.watches.m = map[string]*leaseWatch{}
	}
	c.watches.m[server] = w
	return nil
}

func (c *Coordinator) healthy(w *leaseWatch) bool {
	if !w.informer.HasSynced() {
		return false
	}
	last := w.lastErr.Load()
	return last == 0 || c.now().Sub(time.Unix(0, last)) > watchErrorGrace
}

func (c *Coordinator) existing(server string) *leaseWatch {
	c.watches.mu.Lock()
	defer c.watches.mu.Unlock()
	return c.watches.m[server]
}

func (c *Coordinator) watchSynced(server string) bool {
	w := c.existing(server)
	return w != nil && w.informer.HasSynced()
}

func (c *Coordinator) watchHealthy(server string) bool {
	w := c.existing(server)
	return w != nil && c.healthy(w)
}
