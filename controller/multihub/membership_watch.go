package multihub

import (
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// MembershipWatcher keeps the last valid HubMembership in a namespace (the
// object named HubMembershipName). An invalid version keeps the previous one
// (the status quo) and is reported by LastError; no object at all means no
// membership is declared, i.e. today's behavior.
type MembershipWatcher struct {
	stop chan struct{}

	mu      sync.Mutex
	current Membership
	ok      bool
	lastErr error
}

// NewMembershipWatcher starts watching; Stop stops it.
func NewMembershipWatcher(client dynamic.Interface, namespace string) *MembershipWatcher {
	w := &MembershipWatcher{stop: make(chan struct{})}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, namespace, nil)
	informer := factory.ForResource(HubMembershipGVR).Informer()
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    w.apply,
		UpdateFunc: func(_, obj any) { w.apply(obj) },
		DeleteFunc: w.remove,
	})
	go informer.Run(w.stop)
	return w
}

func (w *MembershipWatcher) apply(obj any) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u.GetName() != HubMembershipName {
		return
	}
	m, err := ParseMembership(u)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.lastErr = err // keep the last valid membership
		return
	}
	w.current, w.ok, w.lastErr = m, true, nil
}

func (w *MembershipWatcher) remove(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u.GetName() != HubMembershipName {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.current, w.ok, w.lastErr = Membership{}, false, nil
}

// Current is the membership in force; ok is false when none is declared
// (or the watch hasn't synced yet).
func (w *MembershipWatcher) Current() (Membership, bool) {
	if w == nil {
		return Membership{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current, w.ok
}

// LastError is why the latest version was rejected, or nil.
func (w *MembershipWatcher) LastError() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

// Stop stops the watch.
func (w *MembershipWatcher) Stop() {
	if w != nil {
		close(w.stop)
	}
}
