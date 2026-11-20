package multihub

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	"github.com/argoproj/argo-cd/v3/util/env"
)

// Telemetry settings. Every knob is an environment variable on the
// application controller; all have safe defaults.
//
// Warnings (takeover, lost lease, lease errors) are always logged at warn
// level. The knobs only change how much routine traffic is logged, and
// anything turned "off" is demoted to debug level rather than dropped, so
// --loglevel=debug always shows the full picture.
const (
	// Log acquiring and releasing the lease at info (default true).
	EnvLogLifecycle = "ARGOCD_MULTIHUB_LOG_LIFECYCLE"
	// Log every renewal at info; renewals happen on every operation pass, so
	// this is noisy (default false: debug).
	EnvLogRenewals = "ARGOCD_MULTIHUB_LOG_RENEWALS"
	// How refusals (lease held by another hub / settling) are logged:
	//   changes: info when the reason or holder changes for an app, debug for repeats (default)
	//   all:     info every time
	//   off:     debug only
	// Lease errors follow the same dedup, but are never demoted below warn on change.
	EnvLogDenials = "ARGOCD_MULTIHUB_LOG_DENIALS"
	// With LOG_DENIALS=changes, a repeat refusal or error for an app is news
	// again once none has been seen for this long, so a new episode is logged
	// even when it repeats the last one word for word (default 2m; 0 = never).
	EnvLogDenialsResetAfter = "ARGOCD_MULTIHUB_LOG_DENIALS_RESET_AFTER"
	// Emit Kubernetes Events on the Application for takeover, lost lease,
	// lease errors and operations waiting on another hub (default true).
	EnvEvents = "ARGOCD_MULTIHUB_EVENTS"
	// Export argocd_multihub_lease_decisions_total (default true).
	EnvMetrics = "ARGOCD_MULTIHUB_METRICS"
)

type DenialLogMode string

const (
	DenialLogChanges DenialLogMode = "changes"
	DenialLogAll     DenialLogMode = "all"
	DenialLogOff     DenialLogMode = "off"
)

type Telemetry struct {
	LogLifecycle bool
	LogRenewals  bool
	LogDenials   DenialLogMode
	// DenialsResetAfter: a quiet gap after which a repeat is reported again.
	DenialsResetAfter time.Duration
	Events            bool
	Metrics           bool
}

func DefaultTelemetry() Telemetry {
	return Telemetry{LogLifecycle: true, LogDenials: DenialLogChanges, Events: true, Metrics: true}
}

func TelemetryFromEnv() Telemetry {
	d := DefaultTelemetry()
	t := Telemetry{
		LogLifecycle: env.ParseBoolFromEnv(EnvLogLifecycle, d.LogLifecycle),
		LogRenewals:  env.ParseBoolFromEnv(EnvLogRenewals, d.LogRenewals),
		LogDenials:   DenialLogMode(strings.ToLower(env.StringFromEnv(EnvLogDenials, string(d.LogDenials)))),
		Events:       env.ParseBoolFromEnv(EnvEvents, d.Events),
		Metrics:      env.ParseBoolFromEnv(EnvMetrics, d.Metrics),
	}
	switch t.LogDenials {
	case DenialLogChanges, DenialLogAll, DenialLogOff:
	default:
		log.Warnf("%s=%q is not one of changes|all|off; using %q", EnvLogDenials, t.LogDenials, d.LogDenials)
		t.LogDenials = d.LogDenials
	}
	return t
}

// Report says how one lease decision should be surfaced.
type Report struct {
	// LogLevel to log at. Always set; routine traffic that is switched off
	// lands at debug.
	LogLevel log.Level
	// Emit a Kubernetes Event on the Application.
	Event bool
	// The Event is a Warning (otherwise Normal).
	Warning bool
}

// Reporter applies the Telemetry policy. It remembers the last refusal or
// error reported per (app, path) so repeats can be collapsed.
type Reporter struct {
	cfg  Telemetry
	now  func() time.Time
	last sync.Map // appKey|path -> event|holder|reason
}

func NewReporter(cfg Telemetry) *Reporter { return &Reporter{cfg: cfg, now: time.Now} }

// Plan decides how to report an event for app (any stable per-app key) on a
// code path ("auto-sync" or "operation").
func (r *Reporter) Plan(app, path string, ev Event, holder, reason string) Report {
	k := app + "|" + path
	switch ev {
	case EventAcquired, EventRenewed, EventTakeover, EventReleased:
		r.last.Delete(k) // a fresh refusal after this is news again
	}

	switch ev {
	case EventTakeover, EventLost:
		return Report{LogLevel: log.WarnLevel, Event: r.cfg.Events, Warning: true}
	case EventAcquired, EventReleased:
		return Report{LogLevel: level(r.cfg.LogLifecycle)}
	case EventRenewed:
		return Report{LogLevel: level(r.cfg.LogRenewals)}
	case EventHeld, EventSettling:
		changed := r.changed(k, ev, holder, "")
		lvl := log.DebugLevel
		switch r.cfg.LogDenials {
		case DenialLogAll:
			lvl = log.InfoLevel
		case DenialLogChanges:
			if changed {
				lvl = log.InfoLevel
			}
		}
		// A waiting operation is something a user started; tell them why.
		return Report{LogLevel: lvl, Event: r.cfg.Events && changed && path == PathOperation}
	case EventError, EventReleaseError:
		// The reason is part of the key: a different error is news.
		changed := r.changed(k, ev, holder, reason)
		if changed || r.cfg.LogDenials == DenialLogAll {
			return Report{LogLevel: log.WarnLevel, Event: r.cfg.Events && changed, Warning: true}
		}
		return Report{LogLevel: log.DebugLevel}
	}
	return Report{LogLevel: log.DebugLevel}
}

func (r *Reporter) changed(k string, ev Event, holder, reason string) bool {
	v := string(ev) + "|" + holder + "|" + reason
	prev, loaded := r.last.Swap(k, v)
	return !loaded || prev.(string) != v
}

func level(on bool) log.Level {
	if on {
		return log.InfoLevel
	}
	return log.DebugLevel
}

// Code paths that ask for the lease, used as a log field and metric label.
const (
	PathAutoSync  = "auto-sync"
	PathOperation = "operation"
	PathPin       = "pin" // revision pin decisions (event = PinEvent)
)

// DecisionsTotal counts every lease decision by hub, code path and event.
var DecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "argocd_multihub_lease_decisions_total",
	Help: "Multi-hub operation lease decisions, by hub, code path (auto-sync|operation) and event (acquired|renewed|takeover|held|settling|released|lost|error|release_error).",
}, []string{"hub", "path", "event"})

// RegisterMetrics registers the multihub metrics once; repeated calls (e.g.
// one controller per test) are fine.
func RegisterMetrics(reg prometheus.Registerer) error {
	err := reg.Register(DecisionsTotal)
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return nil
	}
	return err
}

// PlanPin decides how to report a revision-pin decision for app. Moves of
// the pin (pinned, advanced, adopted) follow LOG_LIFECYCLE; agreeing with it
// is debug; errors warn when they change, like lease errors. No Kubernetes
// Events: the pin moves on every commit.
func (r *Reporter) PlanPin(app string, ev PinEvent, reason string) Report {
	k := app + "|" + PathPin
	switch ev {
	case PinError:
		if r.changed(k, EventError, "", reason) || r.cfg.LogDenials == DenialLogAll {
			return Report{LogLevel: log.WarnLevel}
		}
		return Report{LogLevel: log.DebugLevel}
	case PinPinned, PinAdvanced, PinAdopted:
		r.last.Delete(k)
		return Report{LogLevel: level(r.cfg.LogLifecycle)}
	default: // kept
		r.last.Delete(k)
		return Report{LogLevel: log.DebugLevel}
	}
}
