// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sync"
)

// PerfEvent is a software perf event attached to a task.
//
// +stateify savable
type PerfEvent struct {
	mu sync.Mutex `state:"nosave"`

	k      *Kernel
	attr   linux.PerfEventAttr
	id     uint64
	target *Task

	enabled bool
	count   uint64
	started ktime.Time
}

// NewPerfEvent creates a counting software event and attaches it to target.
func NewPerfEvent(target *Task, attr linux.PerfEventAttr) *PerfEvent {
	k := target.Kernel()
	ev := &PerfEvent{
		k:      k,
		attr:   attr,
		id:     k.UniqueID(),
		target: target,
	}
	if attr.Bits&linux.PerfBitDisabled == 0 {
		ev.enabled = true
		ev.started = k.MonotonicClock().Now()
	}
	target.AddPerfEvent(ev)
	return ev
}

// AddPerfEvent records ev on t's event list.
func (t *Task) AddPerfEvent(ev *PerfEvent) {
	t.perfMu.Lock()
	defer t.perfMu.Unlock()
	t.perfEvents = append(t.perfEvents, ev)
}

// RemovePerfEvent detaches ev from t's event list.
func (t *Task) RemovePerfEvent(ev *PerfEvent) {
	t.perfMu.Lock()
	defer t.perfMu.Unlock()
	for i, e := range t.perfEvents {
		if e == ev {
			t.perfEvents = append(t.perfEvents[:i], t.perfEvents[i+1:]...)
			return
		}
	}
}

func (t *Task) exitPerfEvents() {
	t.perfMu.Lock()
	events := append([]*PerfEvent(nil), t.perfEvents...)
	t.perfMu.Unlock()
	for _, ev := range events {
		ev.Disable()
	}
}

func (ev *PerfEvent) accumulateLocked() {
	if !ev.enabled {
		return
	}
	now := ev.k.MonotonicClock().Now()
	if delta := now.Sub(ev.started); delta > 0 {
		ev.count += uint64(delta)
	}
	ev.started = now
}

// Enable starts counting.
func (ev *PerfEvent) Enable() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if ev.enabled {
		return
	}
	ev.enabled = true
	ev.started = ev.k.MonotonicClock().Now()
}

// Disable stops counting and folds elapsed time into the count.
func (ev *PerfEvent) Disable() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.accumulateLocked()
	ev.enabled = false
}

// Reset zeros the count.
func (ev *PerfEvent) Reset() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.accumulateLocked()
	ev.count = 0
}

// SetPeriod stores the sample period. Overflow sampling is not implemented.
func (ev *PerfEvent) SetPeriod(period uint64) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.attr.SamplePeriod = period
}

// ID returns the event id.
func (ev *PerfEvent) ID() uint64 {
	return ev.id
}

// Target returns the monitored task.
func (ev *PerfEvent) Target() *Task {
	return ev.target
}

// ReadFormat writes the non-GROUP read_format layout into dst.
//
// Too-small dst returns ENOSPC. FORMAT_GROUP is EINVAL.
func (ev *PerfEvent) ReadFormat(dst []byte) (int, error) {
	rf := ev.attr.ReadFormat
	if rf&linux.PERF_FORMAT_GROUP != 0 {
		return 0, linuxerr.EINVAL
	}
	n := 8
	if rf&linux.PERF_FORMAT_TOTAL_TIME_ENABLED != 0 {
		n += 8
	}
	if rf&linux.PERF_FORMAT_TOTAL_TIME_RUNNING != 0 {
		n += 8
	}
	if rf&linux.PERF_FORMAT_ID != 0 {
		n += 8
	}
	if rf&linux.PERF_FORMAT_LOST != 0 {
		n += 8
	}
	if len(dst) < n {
		return 0, linuxerr.ENOSPC
	}

	ev.mu.Lock()
	ev.accumulateLocked()
	count := ev.count
	id := ev.id
	ev.mu.Unlock()

	off := 0
	put := func(v uint64) {
		hostarch.ByteOrder.PutUint64(dst[off:], v)
		off += 8
	}
	put(count)
	if rf&linux.PERF_FORMAT_TOTAL_TIME_ENABLED != 0 {
		put(count)
	}
	if rf&linux.PERF_FORMAT_TOTAL_TIME_RUNNING != 0 {
		put(count)
	}
	if rf&linux.PERF_FORMAT_ID != 0 {
		put(id)
	}
	if rf&linux.PERF_FORMAT_LOST != 0 {
		put(0)
	}
	return n, nil
}
