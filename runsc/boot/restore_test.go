// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package boot

import (
	"errors"
	"math"
	"os"
	"testing"
	"testing/synctest"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

func TestSaveAsyncDuringRestoreSetup(t *testing.T) {
	l := &Loader{k: &kernel.Kernel{}, state: atomicbitops.FromUint32(uint32(restoringSetup))}
	if err := l.SaveAsync(); err != linuxerr.EBUSY {
		t.Fatalf("SaveAsync() error = %v, want %v", err, linuxerr.EBUSY)
	}
}

func TestGuestFSSaveDuringFullCheckpoint(t *testing.T) {
	l := &Loader{state: atomicbitops.FromUint32(uint32(started))}
	l.lockFullCheckpoint()
	defer l.unlockFullCheckpoint()
	if err := l.FSSave(); !errors.Is(err, linuxerr.EBUSY) {
		t.Fatalf("FSSave() during a full checkpoint outside an application hook = %v, want %v", err, linuxerr.EBUSY)
	}
}

func TestSaveAsyncReportsConversionFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// An uncompressed checkpoint requires all three state and page files.
	files, err := fd.NewFromFiles([]*os.File{file, file})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		defer file.Close()
	}

	synctest.Test(t, func(t *testing.T) {
		l := &Loader{
			k:       &kernel.Kernel{},
			root:    containerInfo{spec: &specs.Spec{}},
			saveFDs: files,
			state:   atomicbitops.FromUint32(uint32(started)),
		}
		if err := l.SaveAsync(); err != nil {
			t.Fatalf("SaveAsync() failed to start: %v", err)
		}
		synctest.Wait()
		if got := l.k.CheckpointGen().Count; got != 1 {
			t.Errorf("checkpoint completed %d times, want 1", got)
		}
		// A completion notification can wake the guest before the checkpoint
		// locks are released. Its one-shot output files are already gone.
		l.lockFullCheckpoint()
		err := l.SaveAsync()
		l.unlockFullCheckpoint()
		if err != linuxerr.ENXIO {
			t.Errorf("SaveAsync() after notification while checkpoint locks are held = %v, want %v", err, linuxerr.ENXIO)
		}
		if err := l.SaveAsync(); err != linuxerr.ENXIO {
			t.Errorf("second SaveAsync() error = %v, want %v", err, linuxerr.ENXIO)
		}
		if got := l.k.CheckpointGen().Count; got != 1 {
			t.Errorf("checkpoint completion count after retries = %d, want 1", got)
		}
	})
}

func TestSaveConversionFailureNotification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		state         loaderState
		pending       bool
		wantBusy      bool
		wantGenerated uint32
	}{
		{
			name:          "AdmittedCheckpoint",
			state:         started,
			wantGenerated: 1,
		},
		{
			name:     "RestoreHookInProgress",
			state:    restoringHook,
			wantBusy: true,
		},
		{
			name:          "BackgroundPagesLoading",
			state:         restoringReady,
			wantGenerated: 1,
		},
		{
			name:          "PendingCheckpointCleanup",
			state:         started,
			pending:       true,
			wantBusy:      true,
			wantGenerated: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &kernel.Kernel{}
			l := &Loader{k: k, state: atomicbitops.FromUint32(uint32(tc.state))}
			if tc.pending {
				l.checkpointResumeStage.Store(uint32(control.SaveExecPrepared))
			}

			// The admitted case has no output file, so conversion fails
			// before checkpoint preparation starts.
			err := l.save(&controlapi.SaveOpts{})
			if gotBusy := errors.Is(err, linuxerr.EBUSY); gotBusy != tc.wantBusy {
				t.Fatalf("save() error = %v, EBUSY = %t, want EBUSY = %t", err, gotBusy, tc.wantBusy)
			}
			if err == nil {
				t.Fatal("save() succeeded without an output file")
			}
			if got := k.CheckpointGen().Count; got != tc.wantGenerated {
				t.Errorf("checkpoint generation = %d, want %d", got, tc.wantGenerated)
			}
		})
	}
}

func TestTSCOffsetFromSnapshot(t *testing.T) {
	const (
		ghz3     = uint64(3_000_000_000)
		nsPerDay = uint64(86_400_000_000_000)
	)
	for _, tc := range []struct {
		name         string
		baseCycle    uint64
		baseFreq     uint64
		baseRealtime uint64
		curCycle     uint64
		curRealtime  uint64
		want         uint64
		wantOK       bool
	}{
		{
			name:         "NoTimeElapsed",
			baseCycle:    1000,
			baseFreq:     ghz3,
			baseRealtime: 5_000,
			curCycle:     400,
			curRealtime:  5_000,
			want:         600,
			wantOK:       true,
		},
		{
			name:         "RealtimeBehindSnapshot",
			baseCycle:    1000,
			baseFreq:     ghz3,
			baseRealtime: 10_000,
			curCycle:     400,
			curRealtime:  5_000,
			want:         600,
			wantOK:       true,
		},
		{
			name:         "OneSecond",
			baseCycle:    1000,
			baseFreq:     ghz3,
			baseRealtime: 0,
			curCycle:     1000,
			curRealtime:  1_000_000_000,
			want:         ghz3,
			wantOK:       true,
		},
		{
			// deltaNs * baseFreq overflows 64 bits here.
			name:         "OneDay",
			baseCycle:    1_000_000,
			baseFreq:     ghz3,
			baseRealtime: 1_000,
			curCycle:     1_000_000,
			curRealtime:  1_000 + nsPerDay,
			want:         86_400 * ghz3,
			wantOK:       true,
		},
		{
			name:         "NegativeOffset",
			baseCycle:    1000,
			baseFreq:     ghz3,
			baseRealtime: 0,
			curCycle:     5000,
			curRealtime:  0,
			want:         ^uint64(4000) + 1,
			wantOK:       true,
		},
		{
			name:         "ElapsedCyclesOverflow",
			baseCycle:    0,
			baseFreq:     math.MaxUint64,
			baseRealtime: 0,
			curCycle:     0,
			curRealtime:  2_000_000_000,
			wantOK:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tscOffsetFromSnapshot(tc.baseCycle, tc.baseFreq, tc.baseRealtime, tc.curCycle, tc.curRealtime)
			if ok != tc.wantOK {
				t.Fatalf("tscOffsetFromSnapshot() ok = %t, want %t", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("tscOffsetFromSnapshot() = %d, want %d", got, tc.want)
			}
		})
	}
}
