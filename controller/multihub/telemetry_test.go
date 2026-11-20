package multihub

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelemetryFromEnvDefaults(t *testing.T) {
	assert.Equal(t, DefaultTelemetry(), TelemetryFromEnv())
	d := DefaultTelemetry()
	assert.True(t, d.LogLifecycle)
	assert.False(t, d.LogRenewals)
	assert.Equal(t, DenialLogChanges, d.LogDenials)
	assert.Equal(t, 2*time.Minute, d.DenialsResetAfter)
	assert.True(t, d.Events)
	assert.True(t, d.Metrics)
}

func TestTelemetryFromEnvOverrides(t *testing.T) {
	t.Setenv(EnvLogLifecycle, "false")
	t.Setenv(EnvLogRenewals, "true")
	t.Setenv(EnvLogDenials, "ALL")
	t.Setenv(EnvLogDenialsResetAfter, "30s")
	t.Setenv(EnvEvents, "false")
	t.Setenv(EnvMetrics, "false")
	assert.Equal(t, Telemetry{LogLifecycle: false, LogRenewals: true, LogDenials: DenialLogAll, DenialsResetAfter: 30 * time.Second, Events: false, Metrics: false}, TelemetryFromEnv())
}

func TestTelemetryFromEnvResetAfter(t *testing.T) {
	t.Setenv(EnvLogDenialsResetAfter, "0")
	assert.Equal(t, time.Duration(0), TelemetryFromEnv().DenialsResetAfter, "0 = never reset")
	t.Setenv(EnvLogDenialsResetAfter, "soon")
	assert.Equal(t, 2*time.Minute, TelemetryFromEnv().DenialsResetAfter, "invalid falls back to the default")
}

func TestTelemetryFromEnvRejectsUnknownDenialMode(t *testing.T) {
	t.Setenv(EnvLogDenials, "sometimes")
	assert.Equal(t, DenialLogChanges, TelemetryFromEnv().LogDenials)
}

func TestWarningsAlwaysWarnAndRaiseWarningEvents(t *testing.T) {
	quiet := Telemetry{LogDenials: DenialLogOff, Events: true}
	r := NewReporter(quiet)
	for _, ev := range []Event{EventTakeover, EventLost} {
		p := r.Plan("argocd/app", PathOperation, ev, "hub-b", "")
		assert.Equal(t, log.WarnLevel, p.LogLevel, ev)
		assert.True(t, p.Event, ev)
		assert.True(t, p.Warning, ev)
	}
}

func TestEventsCanBeDisabled(t *testing.T) {
	r := NewReporter(Telemetry{LogDenials: DenialLogChanges, Events: false})
	assert.False(t, r.Plan("a", PathOperation, EventTakeover, "", "").Event)
	assert.False(t, r.Plan("a", PathOperation, EventHeld, "hub-b", "").Event)
	assert.False(t, r.Plan("a", PathAutoSync, EventError, "", "boom").Event)
}

func TestLifecycleAndRenewalLevels(t *testing.T) {
	on := NewReporter(Telemetry{LogLifecycle: true, LogRenewals: true})
	off := NewReporter(Telemetry{})
	for _, ev := range []Event{EventAcquired, EventReleased} {
		assert.Equal(t, log.InfoLevel, on.Plan("a", PathOperation, ev, "", "").LogLevel)
		assert.Equal(t, log.DebugLevel, off.Plan("a", PathOperation, ev, "", "").LogLevel, "off means debug, not dropped")
	}
	assert.Equal(t, log.InfoLevel, on.Plan("a", PathOperation, EventRenewed, "", "").LogLevel)
	assert.Equal(t, log.DebugLevel, off.Plan("a", PathOperation, EventRenewed, "", "").LogLevel)
	assert.False(t, on.Plan("a", PathOperation, EventAcquired, "", "").Event, "no Events for routine lifecycle")
}

func TestDenialsChangesMode(t *testing.T) {
	r := NewReporter(Telemetry{LogDenials: DenialLogChanges, Events: true})

	first := r.Plan("a", PathAutoSync, EventHeld, "hub-a", "")
	assert.Equal(t, log.InfoLevel, first.LogLevel)
	assert.False(t, first.Event, "auto-sync refusals never raise Events")

	assert.Equal(t, log.DebugLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel, "repeat collapsed")
	assert.Equal(t, log.InfoLevel, r.Plan("a", PathAutoSync, EventSettling, "hub-a", "").LogLevel, "different event is news")
	assert.Equal(t, log.InfoLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-c", "").LogLevel, "different holder is news")
	assert.Equal(t, log.InfoLevel, r.Plan("b", PathAutoSync, EventHeld, "hub-c", "").LogLevel, "tracked per app")

	// Getting the lease resets the dedup; the next refusal is reported again.
	r.Plan("a", PathAutoSync, EventAcquired, "", "")
	assert.Equal(t, log.InfoLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-c", "").LogLevel)
}

func TestWaitingOperationRaisesOneNormalEvent(t *testing.T) {
	r := NewReporter(Telemetry{LogDenials: DenialLogChanges, Events: true})
	p := r.Plan("a", PathOperation, EventHeld, "hub-b", "")
	assert.True(t, p.Event)
	assert.False(t, p.Warning)
	assert.False(t, r.Plan("a", PathOperation, EventHeld, "hub-b", "").Event, "not repeated every 5s")
}

func TestDenialsAllAndOffModes(t *testing.T) {
	all := NewReporter(Telemetry{LogDenials: DenialLogAll})
	off := NewReporter(Telemetry{LogDenials: DenialLogOff})
	for range 3 {
		assert.Equal(t, log.InfoLevel, all.Plan("a", PathAutoSync, EventHeld, "hub-b", "").LogLevel)
		assert.Equal(t, log.DebugLevel, off.Plan("a", PathAutoSync, EventHeld, "hub-b", "").LogLevel)
	}
}

func TestErrorsWarnOnChangeEvenWhenDenialsOff(t *testing.T) {
	r := NewReporter(Telemetry{LogDenials: DenialLogOff, Events: true})
	p := r.Plan("a", PathAutoSync, EventError, "", "connection refused")
	assert.Equal(t, log.WarnLevel, p.LogLevel)
	assert.True(t, p.Event)
	assert.True(t, p.Warning)
	assert.Equal(t, log.DebugLevel, r.Plan("a", PathAutoSync, EventError, "", "connection refused").LogLevel, "same error collapsed")
	assert.Equal(t, log.WarnLevel, r.Plan("a", PathAutoSync, EventError, "", "forbidden").LogLevel, "new error reported")
}

func TestRegisterMetricsIsIdempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, RegisterMetrics(reg))
	require.NoError(t, RegisterMetrics(reg))
}

func TestDecisionsCarryEvents(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	ctx := t.Context()

	d, _ := a.AcquireForOperation(ctx, server, key)
	assert.Equal(t, EventAcquired, d.Event)
	d, _ = a.AcquireForOperation(ctx, server, key)
	assert.Equal(t, EventRenewed, d.Event)
	d, _ = b.AcquireForAutoSync(ctx, server, key)
	assert.Equal(t, EventHeld, d.Event)

	clk.advance(61 * time.Second)
	d, _ = b.AcquireForOperation(ctx, server, key)
	assert.Equal(t, EventTakeover, d.Event)

	require.NoError(t, b.Release(ctx, server, key))
	d, _ = a.AcquireForAutoSync(ctx, server, key)
	assert.Equal(t, EventSettling, d.Event)

	var disabled *Coordinator
	d, _ = disabled.AcquireForAutoSync(ctx, server, key)
	assert.Equal(t, EventDisabled, d.Event)
}

func TestPinPlanLevels(t *testing.T) {
	on := NewReporter(Telemetry{LogLifecycle: true, Events: true})
	off := NewReporter(Telemetry{})
	for _, ev := range []PinEvent{PinPinned, PinAdvanced, PinAdopted} {
		assert.Equal(t, log.InfoLevel, on.PlanPin("a", ev, "").LogLevel, ev)
		assert.Equal(t, log.DebugLevel, off.PlanPin("a", ev, "").LogLevel, "%s with LOG_LIFECYCLE off", ev)
		assert.False(t, on.PlanPin("a", ev, "").Event, "pin moves raise no Kubernetes Event")
	}
	assert.Equal(t, log.DebugLevel, on.PlanPin("a", PinKept, "").LogLevel, "agreeing with the pin is routine")
}

func TestPinPlanErrorsWarnOnChange(t *testing.T) {
	r := NewReporter(Telemetry{LogDenials: DenialLogOff})
	assert.Equal(t, log.WarnLevel, r.PlanPin("a", PinError, "lookup failed").LogLevel)
	assert.Equal(t, log.DebugLevel, r.PlanPin("a", PinError, "lookup failed").LogLevel, "same error collapsed")
	assert.Equal(t, log.WarnLevel, r.PlanPin("a", PinError, "forbidden").LogLevel, "new error reported")
	assert.Equal(t, log.WarnLevel, r.PlanPin("b", PinError, "forbidden").LogLevel, "tracked per app")

	r.PlanPin("a", PinKept, "") // recovering resets the dedup
	assert.Equal(t, log.WarnLevel, r.PlanPin("a", PinError, "forbidden").LogLevel)

	all := NewReporter(Telemetry{LogDenials: DenialLogAll})
	for range 2 {
		assert.Equal(t, log.WarnLevel, all.PlanPin("a", PinError, "lookup failed").LogLevel)
	}
}

// fakeNow is a settable clock for the Reporter.
type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time          { return f.t }
func (f *fakeNow) advance(d time.Duration) { f.t = f.t.Add(d) }
func reporterAt(cfg Telemetry) (*Reporter, *fakeNow) {
	clk := &fakeNow{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	r := NewReporter(cfg)
	r.now = clk.now
	return r, clk
}

// The E7 finding (research/2026-11-14-c1-revision-pin.md): hub-b was refused
// by hub-a 7 minutes after an identical refusal, and every refusal went to
// debug because the dedup never forgot the earlier episode.
func TestDenialsRepeatIsNewsAfterQuietGap(t *testing.T) {
	r, clk := reporterAt(Telemetry{LogDenials: DenialLogChanges, DenialsResetAfter: 2 * time.Minute})

	assert.Equal(t, log.InfoLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel)
	// A continuous episode (rechecks every 15s) stays collapsed indefinitely:
	// the gap is measured from the last refusal seen, not the last one logged.
	for range 40 {
		clk.advance(15 * time.Second)
		assert.Equal(t, log.DebugLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel)
	}

	clk.advance(7 * time.Minute)
	assert.Equal(t, log.InfoLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel, "new episode after a quiet gap")
	clk.advance(15 * time.Second)
	assert.Equal(t, log.DebugLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel, "and collapsed again within it")
}

func TestOperationWaitEventRepeatsAfterQuietGap(t *testing.T) {
	r, clk := reporterAt(Telemetry{LogDenials: DenialLogChanges, DenialsResetAfter: 2 * time.Minute, Events: true})
	assert.True(t, r.Plan("a", PathOperation, EventHeld, "hub-b", "").Event)
	clk.advance(5 * time.Second)
	assert.False(t, r.Plan("a", PathOperation, EventHeld, "hub-b", "").Event)
	clk.advance(3 * time.Minute)
	assert.True(t, r.Plan("a", PathOperation, EventHeld, "hub-b", "").Event, "a later wait is a new user-visible wait")
}

func TestErrorsWarnAgainAfterQuietGap(t *testing.T) {
	r, clk := reporterAt(Telemetry{LogDenials: DenialLogChanges, DenialsResetAfter: time.Minute})
	assert.Equal(t, log.WarnLevel, r.Plan("a", PathAutoSync, EventError, "", "forbidden").LogLevel)
	clk.advance(30 * time.Second)
	assert.Equal(t, log.DebugLevel, r.Plan("a", PathAutoSync, EventError, "", "forbidden").LogLevel)
	clk.advance(61 * time.Second)
	assert.Equal(t, log.WarnLevel, r.Plan("a", PathAutoSync, EventError, "", "forbidden").LogLevel)

	assert.Equal(t, log.WarnLevel, r.PlanPin("a", PinError, "lookup failed").LogLevel)
	clk.advance(30 * time.Second)
	assert.Equal(t, log.DebugLevel, r.PlanPin("a", PinError, "lookup failed").LogLevel)
	clk.advance(61 * time.Second)
	assert.Equal(t, log.WarnLevel, r.PlanPin("a", PinError, "lookup failed").LogLevel, "pin errors too")
}

func TestResetAfterZeroNeverForgets(t *testing.T) {
	r, clk := reporterAt(Telemetry{LogDenials: DenialLogChanges})
	assert.Equal(t, log.InfoLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel)
	clk.advance(24 * time.Hour)
	assert.Equal(t, log.DebugLevel, r.Plan("a", PathAutoSync, EventHeld, "hub-a", "").LogLevel)
}
