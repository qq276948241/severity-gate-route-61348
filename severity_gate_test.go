// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zap_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// countingMarshaler records whether its fields were ever serialized, so tests
// can prove that dropped entries are never formatted.
type countingMarshaler struct {
	mu    *sync.Mutex
	calls *int
}

func (c countingMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.calls++
	return nil
}

func newGateLogger(level zapcore.Level, opts ...zap.Option) (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(level)
	return zap.New(core, opts...), logs
}

// The level gate is a single decision point: entries below the current level
// are rejected by Check itself, before any entry is constructed.
func TestSeverityGate_CheckRejectsBelowLevel(t *testing.T) {
	logger, logs := newGateLogger(zap.WarnLevel)

	assert.Nil(t, logger.Check(zap.DebugLevel, "debug"), "debug must be rejected at the gate")
	assert.Nil(t, logger.Check(zap.InfoLevel, "info"), "info must be rejected at the gate")
	require.NotNil(t, logger.Check(zap.WarnLevel, "warn"), "warn is exactly at the gate and must pass")
	require.NotNil(t, logger.Check(zap.ErrorLevel, "error"), "error is above the gate and must pass")

	logger.Check(zap.WarnLevel, "warn").Write()
	logger.Check(zap.ErrorLevel, "error").Write()
	assert.Equal(t, 2, logs.Len(), "only entries at or above the gate may be written")
}

// Dropped entries must be discarded before formatting: encoders and field
// marshalers must never run for them.
func TestSeverityGate_DroppedEntriesAreNotFormatted(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	field := zap.Object("obj", countingMarshaler{mu: &mu, calls: &calls})

	var sink bytes.Buffer
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(zapcore.EncoderConfig{MessageKey: "msg"}),
		zapcore.AddSync(&sink),
		zap.WarnLevel,
	)
	logger := zap.New(core)

	logger.Debug("dropped debug", field)
	logger.Info("dropped info", field)
	logger.Warn("kept warn", field)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls, "only the kept entry may be formatted")
	assert.Contains(t, sink.String(), "kept warn")
	assert.NotContains(t, sink.String(), "dropped")
}

// Entries at exactly the current level are kept.
func TestSeverityGate_EqualLevelIsKept(t *testing.T) {
	for _, lvl := range []zapcore.Level{
		zap.DebugLevel, zap.InfoLevel, zap.WarnLevel, zap.ErrorLevel,
	} {
		logger, logs := newGateLogger(lvl)
		logger.Log(lvl, "at the gate")
		assert.Equal(t, 1, logs.Len(), "level %v: entry at exactly the gate level must be kept", lvl)
	}
}

// Choosing a higher or lower level for one call affects only that call; the
// logger's own level is unchanged for the next call.
func TestSeverityGate_PerCallLevelDoesNotMutateLogger(t *testing.T) {
	logger, logs := newGateLogger(zap.WarnLevel)

	logger.Log(zap.ErrorLevel, "temporarily raised")
	logger.Log(zap.InfoLevel, "temporarily lowered")
	assert.Equal(t, zap.WarnLevel, logger.Level(), "per-call levels must not change the logger's level")

	logger.Log(zap.WarnLevel, "back to normal")
	assert.Equal(t, 2, logs.Len(), "the next call must use the logger's own level")
	assert.Equal(t, "temporarily raised", logs.AllUntimed()[0].Message)
	assert.Equal(t, "back to normal", logs.AllUntimed()[1].Message)
}

// With AddCaller, kept entries carry caller information; dropped entries
// produce nothing at all, so no caller lookup is ever performed for them.
func TestSeverityGate_CallerOnlyForKeptEntries(t *testing.T) {
	logger, logs := newGateLogger(zap.WarnLevel, zap.AddCaller())

	logger.Info("dropped info")
	logger.Warn("kept warn")

	entries := logs.AllUntimed()
	require.Len(t, entries, 1, "dropped entries must not reach the output")
	assert.True(t, entries[0].Caller.Defined, "kept entries must carry caller information")
	assert.NotEmpty(t, entries[0].Caller.File)
	assert.NotZero(t, entries[0].Caller.Line)
}

// When the caller cannot be determined, the entry must say so explicitly
// rather than fabricating a file or line.
func TestSeverityGate_UnavailableCallerIsExplicit(t *testing.T) {
	caller := zapcore.NewEntryCaller(0, "", 0, false)
	assert.False(t, caller.Defined, "caller must be marked as not defined")
	assert.Equal(t, "undefined", caller.String(), "unavailable caller must render as an explicit marker")
	assert.Equal(t, "undefined", caller.TrimmedPath())
}

// Unknown level names are rejected when set, not at the first log call, and
// an unrecognized name never falls back to a default level.
func TestSeverityGate_UnknownLevelNameRejectedAtSetTime(t *testing.T) {
	_, err := zap.ParseAtomicLevel("not-a-level")
	assert.Error(t, err, "unknown level names must be rejected at parse time")

	lvl := zap.NewAtomicLevelAt(zap.WarnLevel)
	err = lvl.UnmarshalText([]byte("not-a-level"))
	assert.Error(t, err, "unknown level names must be rejected at set time")
	assert.Equal(t, zap.WarnLevel, lvl.Level(), "a rejected name must not reset the level to a default")

	var core zapcore.Level
	assert.Error(t, core.UnmarshalText([]byte("bogus")), "zapcore.Level must reject unknown names")
}

// A child logger may be stricter than its parent, but never looser; looser
// requests are rejected when the child is created.
func TestSeverityGate_ChildMayBeStricterNotLooser(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)

	stricter, err := zapcore.NewIncreaseLevelCore(core, zap.ErrorLevel)
	require.NoError(t, err, "a stricter child must be allowed")
	zap.New(stricter).Error("kept error")
	zap.New(stricter).Warn("dropped warn")
	assert.Equal(t, 1, logs.Len(), "stricter child must filter below its own level")

	_, err = zapcore.NewIncreaseLevelCore(core, zap.DebugLevel)
	assert.Error(t, err, "a looser child must be rejected at creation time")

	errorOut := &bytes.Buffer{}
	parent := zap.New(core, zap.ErrorOutput(zapcore.AddSync(errorOut)))
	child := parent.WithOptions(zap.IncreaseLevel(zap.DebugLevel))
	child.Info("still dropped")
	assert.Equal(t, 1, logs.Len(), "a rejected loosening must leave the logger unchanged")
	assert.Contains(t, errorOut.String(), "failed to IncreaseLevel",
		"loosening attempts must be reported when the option is applied")
}

// IncreaseLevel applies to the derived logger only; the parent keeps its own
// level.
func TestSeverityGate_IncreaseLevelScopedToDerivedLogger(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)

	parent := zap.New(core)
	child := parent.WithOptions(zap.IncreaseLevel(zap.ErrorLevel))

	child.Info("dropped on child")
	child.Error("kept on child")
	parent.Info("kept on parent")

	messages := []string{}
	for _, e := range logs.AllUntimed() {
		messages = append(messages, e.Message)
	}
	assert.Equal(t, []string{"kept on child", "kept on parent"}, messages)
}

// Concurrent writers at different levels are each gated by their own level.
func TestSeverityGate_ConcurrentLevelsGatedIndependently(t *testing.T) {
	logger, logs := newGateLogger(zap.WarnLevel)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				logger.Debug("dropped debug")
				logger.Info("dropped info")
				logger.Warn("kept warn")
				logger.Error("kept error")
			}
		}()
	}
	wg.Wait()

	for _, e := range logs.AllUntimed() {
		assert.GreaterOrEqual(t, e.Level, zap.WarnLevel, "no entry below the gate may appear at the output")
	}
	assert.Equal(t, 8*50*2, logs.Len(), "every entry at or above the gate must be written")
}

// Each log call is gated by the level in effect when the call starts: a level
// change made after Check does not retroactively drop the entry, and later
// calls see the new level.
func TestSeverityGate_LevelSnapshotAtCheckTime(t *testing.T) {
	atomic := zap.NewAtomicLevelAt(zap.InfoLevel)
	core, logs := observer.New(atomic)
	logger := zap.New(core)

	ce := logger.Check(zap.InfoLevel, "started under info")
	require.NotNil(t, ce, "entry must pass the gate in effect when the call starts")

	atomic.SetLevel(zap.ErrorLevel)
	ce.Write()
	assert.Equal(t, 1, logs.Len(), "an in-flight entry finishes under the gate it started with")

	assert.Nil(t, logger.Check(zap.InfoLevel, "after raise"), "subsequent calls use the new gate")
	logger.Error("after raise")
	assert.Equal(t, 2, logs.Len())

	// Concurrent level updates must not race with concurrent logging.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(l zapcore.Level) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				atomic.SetLevel(l)
			}
		}(zapcore.Level(i))
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				logger.Log(zap.WarnLevel, "concurrent warn")
			}
		}()
	}
	wg.Wait()
}
