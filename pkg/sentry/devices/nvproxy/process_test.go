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
	"math"
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy/nvconf"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/state"
)

func TestPIDInfoVideoMemoryUsageABI(t *testing.T) {
	fixture := [48]byte{
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22,
		0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33,
		0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
		0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
		0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66,
	}
	want := nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{
		MemPrivate:              0x1111111111111111,
		MemSharedOwned:          0x2222222222222222,
		MemSharedDuped:          0x3333333333333333,
		ProtectedMemPrivate:     0x4444444444444444,
		ProtectedMemSharedOwned: 0x5555555555555555,
		ProtectedMemSharedDuped: 0x6666666666666666,
	}

	var info nvgpu.NV2080_CTRL_GPU_PID_INFO
	info.Data.UnmarshalBytes(fixture[:])
	if got := info.VideoMemoryUsage(); got != want {
		t.Fatalf("decoded video-memory usage = %+v, want %+v", got, want)
	}
	if got := reflect.TypeFor[nvgpu.NV2080_CTRL_GPU_PID_INFO]().Align(); got != 8 {
		t.Fatalf("PID-info alignment = %d, want 8", got)
	}

	var encodedInfo nvgpu.NV2080_CTRL_GPU_PID_INFO
	encodedInfo.SetVideoMemoryUsage(want)
	var encoded [48]byte
	encodedInfo.Data.MarshalBytes(encoded[:])
	if !bytes.Equal(encoded[:], fixture[:]) {
		t.Fatalf("encoded video-memory usage = %x, want %x", encoded, fixture)
	}
}

func TestProcessAccountingFollowsObjectTeardown(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{abi: &driverABI{processAccounting: true}, capsEnabled: nvconf.CapGraphics, clients: make(map[nvgpu.Handle]*rootClient)}
	newClient := func(id uint32) *rootClient {
		fd := &frontendFD{dev: &frontendDevice{nvp: nvp}, hostFD: -1}
		client := &rootClient{resources: make(map[nvgpu.Handle]*object), params: capturedRmAllocParams{fd: fd}}
		client.objsMu.Lock()
		nvp.objAdd(ctx, fd, client, nvgpu.Handle{Val: id}, nvgpu.NV01_ROOT_CLIENT, client, nvgpu.Handle{})
		nvp.objAdd(ctx, fd, client, nvgpu.Handle{Val: 10}, nvgpu.NV01_DEVICE_0, &rmAllocObject{}, client.handle)
		client.objsMu.Unlock()
		nvp.clients[client.handle] = client
		return client
	}
	original, importer := newClient(1), newClient(2)
	original.objsMu.Lock()
	nvp.objAdd(ctx, original.params.fd, original, nvgpu.Handle{Val: 20}, nvgpu.NV01_MEMORY_LOCAL_USER, &miscObject{}, nvgpu.Handle{Val: 10})
	original.objsMu.Unlock()
	source := original.resources[nvgpu.Handle{Val: 20}]
	// The host allocation boundary is exercised by the GPU workload. This
	// fixture supplies its verified size while testing the owning object API's
	// duplication and recursive teardown behavior without a host device.
	nvp.processMu.Lock()
	nvp.processObjects[source].memory = newProcessMemory(64<<20, false)
	nvp.processObjects[source].memoryKnown = true
	nvp.processMu.Unlock()
	original.objsMu.Lock()
	importer.objsMu.Lock()
	nvp.objDup(ctx, importer, original, nvgpu.Handle{Val: 30}, nvgpu.Handle{Val: 10}, source.handle)
	importer.objsMu.Unlock()
	original.objsMu.Unlock()
	destination := importer.resources[nvgpu.Handle{Val: 30}]
	nvp.processMu.Lock()
	if got := nvp.processObjects[destination].memory.usage().MemSharedDuped; got != 64<<20 {
		t.Fatalf("imported memory = %d, want 64 MiB duplicated", got)
	}
	nvp.processMu.Unlock()
	original.objsMu.Lock()
	deferred := nvp.objFree(ctx, original, original.handle)
	original.objsMu.Unlock()
	for _, release := range deferred {
		release()
	}
	nvp.processMu.Lock()
	if _, ok := nvp.processObjects[source]; ok {
		t.Fatal("source record survived client teardown")
	}
	if got := nvp.processObjects[destination].memory.usage().MemPrivate; got != 64<<20 {
		t.Fatalf("surviving import = %d, want 64 MiB private", got)
	}
	nvp.processMu.Unlock()
	importer.objsMu.Lock()
	deferred = nvp.objFree(ctx, importer, importer.handle)
	importer.objsMu.Unlock()
	for _, release := range deferred {
		release()
	}
	nvp.processMu.Lock()
	defer nvp.processMu.Unlock()
	if len(nvp.processObjects) != 0 {
		t.Fatalf("%d records survived final client teardown", len(nvp.processObjects))
	}
}

func TestProcessAccountingMetadataSerialization(t *testing.T) {
	creator := &kernel.ThreadGroup{}
	device := &rmAllocObject{}
	first, second := &object{}, &object{}
	memory := newProcessMemory(4096, false)
	records := map[*object]*gpuProcessObject{
		first:  {object: first, device: device, creator: creator, memory: memory, memoryKnown: true},
		second: {object: second, device: device, creator: creator, memory: memory.duplicate(), memoryKnown: true},
	}
	var saved bytes.Buffer
	if _, err := state.Save(context.Background(), &saved, &records); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	var restored map[*object]*gpuProcessObject
	if _, err := state.Load(context.Background(), bytes.NewReader(saved.Bytes()), &restored); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("restored %d objects, want 2", len(restored))
	}
	var owner, alias *gpuProcessObject
	for key, record := range restored {
		if record.object != key {
			t.Fatal("restored map key lost its object identity")
		}
		if record.memory.usage().MemSharedOwned != 0 {
			owner = record
		} else {
			alias = record
		}
	}
	if owner == nil || alias == nil || owner.creator == nil || owner.creator != alias.creator || owner.device == nil || owner.device != alias.device {
		t.Fatal("restored records lost shared creator, device, or owner identity")
	}
	owner.memory.release()
	if got := alias.memory.usage().MemPrivate; got != 4096 {
		t.Fatalf("restored ownership transfer = %d, want 4096", got)
	}
}

func TestProcessSnapshotRequiresCompleteDeviceAccounting(t *testing.T) {
	const mib = 1 << 20
	device := &rmAllocObject{}
	var otherParams nvgpu.NV0080_ALLOC_PARAMETERS
	otherParams.DeviceID = 1
	otherDevice := &rmAllocObject{params: capturedRmAllocParams{allocParams: make([]byte, otherParams.SizeBytes())}}
	otherParams.MarshalBytes(otherDevice.params.allocParams)
	owner, consumer := &kernel.ThreadGroup{}, &kernel.ThreadGroup{}
	shared := newProcessMemory(64*mib, false)
	original, imported, private, foreign := &object{class: nvgpu.NV01_MEMORY_LOCAL_USER}, &object{class: nvgpu.NV01_MEMORY_LOCAL_USER}, &object{class: nvgpu.NV01_MEMORY_LOCAL_USER}, &object{class: nvgpu.NV01_MEMORY_LOCAL_USER}
	nvp := &nvproxy{processObjects: map[*object]*gpuProcessObject{
		original: {object: original, device: device, creator: owner, memory: shared, memoryKnown: true},
		imported: {object: imported, device: device, creator: consumer, memory: shared.duplicate(), memoryKnown: true},
		private:  {object: private, device: device, creator: consumer, memory: newProcessMemory(128*mib, false), memoryKnown: true},
		foreign:  {object: foreign, device: otherDevice, creator: owner, memory: newProcessMemory(1024*mib, false), memoryKnown: true},
	}}
	// A kernel-only alias moves the private allocation from private to
	// shared-owned in the driver aggregate. The guest records still see a
	// private handle, and the aggregate owned-byte total remains unchanged.
	scope := gpuProcessScope{memory: nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{MemSharedOwned: 192 * mib, MemSharedDuped: 64 * mib}}
	snapshots, ok := nvp.snapshotProcesses(scope)
	if !ok {
		t.Fatal("complete, matching accounting was unavailable")
	}
	var owned, duplicated, exclusive uint64
	for _, snapshot := range snapshots {
		owned += snapshot.memory.MemSharedOwned
		duplicated += snapshot.memory.MemSharedDuped
		exclusive += snapshot.memory.MemPrivate
	}
	if owned != 64*mib || duplicated != 64*mib || exclusive != 128*mib {
		t.Fatalf("snapshot mixed scopes or lost sharing: private=%d owned=%d duplicated=%d", exclusive, owned, duplicated)
	}
	wrongOwned := scope
	wrongOwned.memory.MemSharedOwned--
	if _, ok := nvp.snapshotProcesses(wrongOwned); ok {
		t.Fatal("accounting with a different owned-byte total was reported")
	}
	wrongDuplicated := scope
	wrongDuplicated.memory.MemSharedDuped--
	if _, ok := nvp.snapshotProcesses(wrongDuplicated); ok {
		t.Fatal("accounting with a different duplicated-byte total was reported")
	}
	nvp.processMu.Lock()
	nvp.processObjects[private].memoryKnown = false
	nvp.processMu.Unlock()
	if _, ok := nvp.snapshotProcesses(scope); ok {
		t.Fatal("partial accounting was reported with an unknown allocation size")
	}
	nvp.processMu.Lock()
	nvp.processObjects[private].memoryKnown = true
	nvp.processAccountingIncomplete = true
	nvp.processMu.Unlock()
	if _, ok := nvp.snapshotProcesses(scope); ok {
		t.Fatal("partial accounting was reported with an untracked import")
	}
}

func TestProcessMemoryComparisonRejectsOverflow(t *testing.T) {
	overflow := nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{
		MemPrivate:     math.MaxUint64,
		MemSharedOwned: 1,
		MemSharedDuped: 1,
	}
	if processMemoryMatchesDriver(overflow, overflow) {
		t.Fatal("overflowing owned-byte totals were accepted")
	}
}
