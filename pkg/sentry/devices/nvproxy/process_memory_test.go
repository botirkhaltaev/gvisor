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

package nvproxy

import (
	"bytes"
	"context"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/state"
)

func TestProcessMemorySharingAndOwnerRelease(t *testing.T) {
	const size = 64 << 20
	for _, protected := range []bool{false, true} {
		original := newProcessMemory(size, protected)
		private := nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{MemPrivate: size}
		owned := nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{MemSharedOwned: size}
		duped := nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{MemSharedDuped: size}
		if protected {
			private.ProtectedMemPrivate = size
			owned.ProtectedMemSharedOwned = size
			duped.ProtectedMemSharedDuped = size
		}
		check := func(label string, record *processMemory, want nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA) {
			t.Helper()
			if got := record.usage(); got != want {
				t.Fatalf("%s (protected=%t): got %+v, want %+v", label, protected, got, want)
			}
		}
		check("private allocation", original, private)
		imported := original.duplicate()
		check("shared allocation owner", original, owned)
		check("imported allocation", imported, duped)
		original.release()
		check("released owner", original, nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{})
		check("surviving allocation", imported, private)
		imported.release()
		check("all handles released", imported, nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{})
	}
}

func TestProcessMemoryDuplicateOwnershipSuccession(t *testing.T) {
	// Driver duplication inserts after the source, rather than after the
	// newest alias or at the end of a global allocation list.
	const size = 4096
	first := newProcessMemory(size, false)
	second := first.duplicate()
	third := second.duplicate()
	fourth := first.duplicate()
	second.release()
	first.release()
	if got := fourth.usage().MemSharedOwned; got != size {
		t.Fatalf("owner successor usage = %d, want %d", got, size)
	}
	if got := third.usage().MemSharedDuped; got != size {
		t.Fatalf("non-owner usage = %d, want %d", got, size)
	}
	fourth.release()
	if got := third.usage().MemPrivate; got != size {
		t.Fatalf("last surviving alias usage = %d, want %d", got, size)
	}
	third.release()
}

func TestProcessMemorySharingSurvivesSerialization(t *testing.T) {
	original := newProcessMemory(8192, true)
	aliases := []*processMemory{original, original.duplicate()}
	var saved bytes.Buffer
	if _, err := state.Save(context.Background(), &saved, &aliases); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	for range 2 {
		var restored []*processMemory
		if _, err := state.Load(context.Background(), bytes.NewReader(saved.Bytes()), &restored); err != nil {
			t.Fatalf("state.Load: %v", err)
		}
		if len(restored) != 2 {
			t.Fatalf("restored %d aliases, want 2", len(restored))
		}
		if got := restored[0].usage().ProtectedMemSharedOwned; got != 8192 {
			t.Fatalf("restored shared owner usage = %d, want 8192", got)
		}
		restored[0].release()
		if got := restored[1].usage().ProtectedMemPrivate; got != 8192 {
			t.Fatalf("restored surviving alias usage = %d, want 8192", got)
		}
		restored[1].release()
	}
}
