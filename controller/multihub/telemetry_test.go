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
	assert.True(t, d.Events)
	assert.True(t, d.Metrics)
}

func TestTelemetryFromEnvOverrides(t *testing.T) {
	t.Setenv(EnvLogLifecycle, "false")
	t.Setenv(EnvLogRenewals, "true")
	t.Setenv(EnvLogDenials, "ALL")
	t.Setenv(EnvEvents, "false")
	t.Setenv(EnvMetrics, "false")
	assert.Equal(t, Telemetry{LogLifecycle: false, LogRenewals: true, LogDenials: DenialLogAll, Events: false, Metrics: false}, TelemetryFromEnv())
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
