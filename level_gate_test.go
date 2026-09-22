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

package zap

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// countingStringer records whether it was ever formatted.
type countingStringer struct{ calls *atomic.Int32 }

func (c countingStringer) String() string {
	c.calls.Add(1)
	return "counting"
}

// Dropped logs must be discarded before any formatting happens.
func TestLevelGate_DroppedBeforeFormatting(t *testing.T) {
	var calls atomic.Int32
	buf := &bytes.Buffer{}
	enc := zapcore.NewJSONEncoder(NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(buf), WarnLevel)
	logger := New(core)

	logger.Debug("dropped", Stringer("s", countingStringer{&calls}))
	logger.Info("dropped", Stringer("s", countingStringer{&calls}))
	assert.Empty(t, buf.String(), "below-level logs must be dropped")
	assert.Equal(t, int32(0), calls.Load(), "dropped logs must not be formatted")

	// Exactly at the current level is kept (and formatted).
	logger.Warn("kept", Stringer("s", countingStringer{&calls}))
	assert.Contains(t, buf.String(), "kept", "at-level logs must be kept")
	assert.Equal(t, int32(1), calls.Load(), "kept logs must be formatted")
}

// Temporarily raising the level for one logger must not change the parent.
func TestLevelGate_IncreaseLevelIsScoped(t *testing.T) {
	errorOut := &bytes.Buffer{}
	opts := []Option{ErrorOutput(zapcore.AddSync(errorOut))}
	withLogger(t, InfoLevel, opts, func(logger *Logger, logs *observer.ObservedLogs) {
		stricter := logger.WithOptions(IncreaseLevel(ErrorLevel))
		stricter.Info("dropped by stricter")
		stricter.Error("kept by stricter")

		// The original logger is unaffected by the temporary increase.
		logger.Info("still info on parent")

		msgs := make([]string, 0)
		for _, e := range logs.AllUntimed() {
			msgs = append(msgs, e.Message)
		}
		assert.Equal(t, []string{"kept by stricter", "still info on parent"}, msgs)
	})
}

// A child logger may be stricter, but a request to be looser than the parent
// must be rejected at creation time.
func TestLevelGate_LooserChildRejected(t *testing.T) {
	errorOut := &bytes.Buffer{}
	opts := []Option{ErrorOutput(zapcore.AddSync(errorOut))}
	withLogger(t, WarnLevel, opts, func(logger *Logger, logs *observer.ObservedLogs) {
		looser := logger.WithOptions(IncreaseLevel(DebugLevel))
		assert.Contains(t, errorOut.String(), "invalid increase level",
			"loosening must be rejected when the option is applied")

		looser.Debug("still dropped")
		assert.Equal(t, 0, logs.Len(), "rejected loosening must not take effect")

		looser.Warn("parent level still applies")
		assert.Equal(t, 1, logs.Len())
	})
}

// With caller annotation on, kept entries carry caller info while dropped
// entries never reach caller collection.
func TestLevelGate_CallerOnlyForKept(t *testing.T) {
	errorOut := &bytes.Buffer{}
	opts := []Option{AddCaller(), ErrorOutput(zapcore.AddSync(errorOut))}
	withLogger(t, WarnLevel, opts, func(logger *Logger, logs *observer.ObservedLogs) {
		logger.Debug("dropped")
		logger.Warn("kept")

		require.Equal(t, 1, logs.Len())
		caller := logs.AllUntimed()[0].Caller
		require.True(t, caller.Defined, "kept entry must carry caller info")
		assert.Contains(t, caller.String(), "level_gate_test.go")
		assert.Empty(t, errorOut.String(), "dropped entries must not trigger caller collection errors")
	})
}

// An unavailable caller is reported as explicitly undefined, not a fake line
// number and not an empty string.
func TestLevelGate_UndefinedCallerIsExplicit(t *testing.T) {
	ec := zapcore.EntryCaller{Defined: false}
	assert.Equal(t, "undefined", ec.String())
	assert.Equal(t, "undefined", ec.FullPath())
	assert.NotEmpty(t, ec.String())
}

// Unknown level names must be rejected at parse/set time and must not fall
// back to a default level.
func TestLevelGate_UnknownLevelNameRejected(t *testing.T) {
	_, err := zapcore.ParseLevel("not-a-level")
	assert.Error(t, err, "unknown level names must be rejected at parse time")

	lvl := NewAtomicLevelAt(WarnLevel)
	assert.Error(t, lvl.UnmarshalText([]byte("not-a-level")))
	assert.Equal(t, WarnLevel, lvl.Level(), "failed parse must not reset the level")

	_, err = ParseAtomicLevel("not-a-level")
	assert.Error(t, err)
}

// Concurrent writes at different levels are each gated by their own level,
// even while the shared level is being updated.
func TestLevelGate_ConcurrentLevels(t *testing.T) {
	lvl := NewAtomicLevelAt(InfoLevel)
	core, logs := observer.New(zapcore.DebugLevel)
	gated, err := zapcore.NewIncreaseLevelCore(core, lvl)
	require.NoError(t, err)
	logger := New(gated)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if i%2 == 0 {
					lvl.SetLevel(InfoLevel)
				} else {
					lvl.SetLevel(WarnLevel)
				}
				logger.Debug("debug")
				logger.Info("info")
				logger.Warn("warn")
			}
		}(i)
	}
	wg.Wait()

	for _, e := range logs.AllUntimed() {
		assert.NotEqual(t, DebugLevel, e.Level,
			"debug must never pass a gate set to info or warn")
	}
}
