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
	"math"
	"math/bits"
	"sort"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

// gpuProcessObject records the provenance required to attribute an object to a
// guest process. device points to captured allocation parameters so that device
// remapping during restore also remaps the accounting scope.
//
// +stateify savable
type gpuProcessObject struct {
	object      *object
	device      *rmAllocObject
	creator     *kernel.ThreadGroup
	memory      *processMemory
	memoryKnown bool
}

// processDevice returns the captured device that owns o, if its ancestry is
// represented by ordinary RM allocations.
//
// Precondition: o.client.objsMu must be locked.
func processDevice(o *object) *rmAllocObject {
	// Bound traversal even if an inconsistent parent graph contains a cycle.
	for remaining := len(o.client.resources); o != nil && remaining > 0; remaining-- {
		if o.class == nvgpu.NV01_DEVICE_0 {
			device, _ := o.impl.(*rmAllocObject)
			return device
		}
		o = o.client.resources[o.parent]
	}
	return nil
}

// trackProcessObject records a successful allocation or duplication. Size
// discovery is read-only and must not change the result of the allocation.
//
// Precondition: o.client.objsMu must be locked. If source is non-nil, its
// client's objsMu must also be locked.
func (nvp *nvproxy) trackProcessObject(fd *frontendFD, o, source *object) {
	if !nvp.abi.processAccounting || o.class.IsRootClient() {
		return
	}
	record := &gpuProcessObject{
		object:  o,
		device:  processDevice(o),
		creator: o.client.creator,
	}
	if record.device == nil && source == nil {
		return
	}
	if o.class == nvgpu.NV01_MEMORY_LOCAL_USER && source == nil {
		if size, ok := processMemorySize(fd, o.client.handle, o.handle); ok {
			record.memory = newProcessMemory(size, false)
			record.memoryKnown = true
		}
	}
	nvp.processMu.Lock()
	defer nvp.processMu.Unlock()
	if source != nil {
		if original := nvp.processObjects[source]; original != nil {
			if record.device == nil {
				record.device = original.device
			}
			if original.memoryKnown {
				record.memory = original.memory.duplicate()
				record.memoryKnown = true
			}
		}
	}
	if nvp.processObjects == nil {
		nvp.processObjects = make(map[*object]*gpuProcessObject)
	}
	nvp.processObjects[o] = record
}

// resolveProcessMemory translates each creator identity once, after the
// accounting lock is released. A reaped or namespace-invisible creator is
// omitted, so PID reuse cannot attach its memory to a different process.
func resolveProcessMemory(pidns *kernel.PIDNamespace, snapshots []gpuProcessSnapshot) (map[uint32]nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA, bool) {
	pids := make(map[*kernel.ThreadGroup]uint32)
	usage := make(map[uint32]nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA)
	for _, snapshot := range snapshots {
		if snapshot.creator == nil {
			continue
		}
		pid, ok := pids[snapshot.creator]
		if !ok {
			pid = uint32(pidns.IDOfThreadGroup(snapshot.creator))
			pids[snapshot.creator] = pid
		}
		if pid == 0 {
			continue
		}
		total := usage[pid]
		if !addProcessMemory(&total, snapshot.memory) {
			return nil, false
		}
		usage[pid] = total
	}
	return usage, true
}

// Precondition: o.client.objsMu must be locked.
func (nvp *nvproxy) untrackProcessObject(o *object) {
	nvp.processMu.Lock()
	defer nvp.processMu.Unlock()
	if record := nvp.processObjects[o]; record != nil {
		if record.memory != nil {
			record.memory.release()
		}
		delete(nvp.processObjects, o)
	}
}

func processDeviceID(device *rmAllocObject) (uint32, bool) {
	if device == nil {
		return 0, false
	}
	var params nvgpu.NV0080_ALLOC_PARAMETERS
	if len(device.params.allocParams) != 0 {
		if len(device.params.allocParams) != params.SizeBytes() {
			return 0, false
		}
		params.UnmarshalBytes(device.params.allocParams)
	}
	return params.DeviceID, true
}

type gpuProcessScope struct {
	deviceID uint32
	memory   nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA
}

func processSingleSubdevice(object *object) bool {
	allocation, ok := object.impl.(*rmAllocObject)
	if !ok {
		return false
	}
	var params nvgpu.NV2080_ALLOC_PARAMETERS
	if len(allocation.params.allocParams) != 0 {
		if len(allocation.params.allocParams) != params.SizeBytes() {
			return false
		}
		params.UnmarshalBytes(allocation.params.allocParams)
	}
	return params.SubDeviceID == 0
}

// processScope validates the caller's tracked client and subdevice before
// consulting driver state. The host query selects only the sentry's process;
// a guest PID is never forwarded to the host driver.
func processScope(fi *frontendIoctlState, control *nvgpu.NVOS54_PARAMETERS) (gpuProcessScope, uint32, error) {
	var scope gpuProcessScope
	nvp := fi.fd.dev.nvp
	client, unlock := nvp.getClientWithLock(fi.ctx, control.HClient)
	if client == nil {
		return scope, nvgpu.NV_ERR_INVALID_CLIENT, nil
	}
	defer unlock()
	object := client.resources[control.HObject]
	if object == nil || object.class != nvgpu.NV20_SUBDEVICE_0 {
		return scope, nvgpu.NV_ERR_INVALID_OBJECT_HANDLE, nil
	}
	if !processSingleSubdevice(object) {
		return scope, nvgpu.NV_ERR_NOT_SUPPORTED, nil
	}
	deviceID, ok := processDeviceID(processDevice(object))
	if !ok {
		return scope, nvgpu.NV_ERR_NOT_SUPPORTED, nil
	}
	info, status, err := processOwnMemory(fi.fd, control.HClient, control.HObject)
	if err != nil || status != nvgpu.NV_OK {
		return scope, status, err
	}
	memory := info.VideoMemoryUsage()
	// MIG and protected allocations need additional scope and eligibility
	// metadata before their memory buckets can be attributed to guest tasks.
	if info.SMCSubscription.ComputeInstanceID != math.MaxUint32 || info.SMCSubscription.GPUInstanceID != math.MaxUint32 ||
		memory.ProtectedMemPrivate != 0 || memory.ProtectedMemSharedOwned != 0 || memory.ProtectedMemSharedDuped != 0 {
		return scope, nvgpu.NV_ERR_NOT_SUPPORTED, nil
	}
	scope.deviceID, scope.memory = deviceID, memory
	return scope, nvgpu.NV_OK, nil
}

type gpuProcessSnapshot struct {
	creator *kernel.ThreadGroup
	class   nvgpu.ClassID
	memory  nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA
}

// NV2080_CTRL_GPU_GET_PIDS_ID_TYPE_VGPU_GUEST from the 610 UAPI.
const nv2080CtrlGpuGetPidsIDTypeVGpuGuest = 1

func addProcessMemory(dst *nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA, src nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA) bool {
	fields := [6]*uint64{&dst.MemPrivate, &dst.MemSharedOwned, &dst.MemSharedDuped, &dst.ProtectedMemPrivate, &dst.ProtectedMemSharedOwned, &dst.ProtectedMemSharedDuped}
	values := [6]uint64{src.MemPrivate, src.MemSharedOwned, src.MemSharedDuped, src.ProtectedMemPrivate, src.ProtectedMemSharedOwned, src.ProtectedMemSharedDuped}
	for index, field := range fields {
		total, carry := bits.Add64(*field, values[index], 0)
		if carry != 0 {
			return false
		}
		*field = total
	}
	return true
}

// processMemoryMatchesDriver validates the aggregate without erasing the
// guest-visible bucket classification. A kernel-only alias can move the same
// bytes from private to shared-owned, so those buckets are combined with an
// overflow check. Duplicated bytes are visible to the guest alias graph and
// must match exactly.
func processMemoryMatchesDriver(actual, expected nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA) bool {
	if actual.MemSharedDuped != expected.MemSharedDuped ||
		actual.ProtectedMemSharedDuped != expected.ProtectedMemSharedDuped {
		return false
	}
	actualOwned, actualCarry := bits.Add64(actual.MemPrivate, actual.MemSharedOwned, 0)
	expectedOwned, expectedCarry := bits.Add64(expected.MemPrivate, expected.MemSharedOwned, 0)
	if actualCarry != 0 || expectedCarry != 0 {
		return false
	}
	actualProtectedOwned, actualProtectedCarry := bits.Add64(actual.ProtectedMemPrivate, actual.ProtectedMemSharedOwned, 0)
	expectedProtectedOwned, expectedProtectedCarry := bits.Add64(expected.ProtectedMemPrivate, expected.ProtectedMemSharedOwned, 0)
	return actualProtectedCarry == 0 && expectedProtectedCarry == 0 &&
		actualOwned == expectedOwned && actualProtectedOwned == expectedProtectedOwned
}

// snapshotProcesses copies accounting state without resolving guest PIDs.
// Namespace lookups acquire TaskSet locks and must occur after processMu is
// released. Unknown metadata or a disagreement with the driver's aggregate
// makes the snapshot unavailable, rather than yielding partial memory totals.
// The returned records keep the guest-visible classification rather than
// adopting the driver's private/shared-owned split for kernel-only aliases.
func (nvp *nvproxy) snapshotProcesses(scope gpuProcessScope) ([]gpuProcessSnapshot, bool) {
	nvp.processMu.Lock()
	defer nvp.processMu.Unlock()
	if nvp.processAccountingIncomplete {
		return nil, false
	}
	var snapshots []gpuProcessSnapshot
	var total nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA
	for _, record := range nvp.processObjects {
		id, ok := processDeviceID(record.device)
		if !ok || id != scope.deviceID {
			continue
		}
		if record.object.class == nvgpu.NV20_SUBDEVICE_0 && !processSingleSubdevice(record.object) {
			return nil, false
		}
		snapshot := gpuProcessSnapshot{creator: record.creator, class: record.object.class}
		if record.object.class == nvgpu.NV01_MEMORY_LOCAL_USER {
			if !record.memoryKnown {
				return nil, false
			}
			snapshot.memory = record.memory.usage()
			if !addProcessMemory(&total, snapshot.memory) {
				return nil, false
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	if !processMemoryMatchesDriver(total, scope.memory) {
		return nil, false
	}
	return snapshots, true
}

func ctrlGpuGetPids(fi *frontendIoctlState, control *nvgpu.NVOS54_PARAMETERS) (uintptr, error) {
	var params nvgpu.NV2080_CTRL_GPU_GET_PIDS_PARAMS
	if control.ParamsSize != uint32(params.SizeBytes()) {
		return 0, linuxerr.EINVAL
	}
	if control.Flags != 0 {
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_INVALID_ARGUMENT)
	}
	if _, err := params.CopyIn(fi.t, addrFromP64(control.Params)); err != nil {
		return 0, err
	}
	switch params.IDType {
	case nvgpu.NV2080_CTRL_GPU_GET_PIDS_ID_TYPE_CLASS:
	case nv2080CtrlGpuGetPidsIDTypeVGpuGuest:
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_NOT_SUPPORTED)
	default:
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_INVALID_ARGUMENT)
	}
	scope, status, err := processScope(fi, control)
	if err != nil {
		return 0, err
	}
	if status != nvgpu.NV_OK {
		return 0, frontendFailWithStatus(fi, control, status)
	}
	// The driver owns class eligibility (ChannelDescendant, MPS, or
	// Subdevice). In particular, a Memory class must not become enumerable
	// merely because its objects also appear in the accounting registry.
	eligible, status, err := processOwnPidHasClass(fi.fd, control.HClient, control.HObject, params.ID)
	if err != nil {
		return 0, err
	}
	if status != nvgpu.NV_OK {
		return 0, frontendFailWithStatus(fi, control, status)
	}
	if !eligible {
		params.PIDTblCount = 0
		clear(params.PIDTbl[:])
		if _, err := params.CopyOut(fi.t, addrFromP64(control.Params)); err != nil {
			return 0, err
		}
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_OK)
	}
	snapshots, ok := fi.fd.dev.nvp.snapshotProcesses(scope)
	if !ok {
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_NOT_SUPPORTED)
	}
	pids := make(map[uint32]struct{})
	for _, snapshot := range snapshots {
		if snapshot.creator == nil || params.ID != uint32(snapshot.class) {
			continue
		}
		if pid := fi.t.PIDNamespace().IDOfThreadGroup(snapshot.creator); pid != 0 {
			pids[uint32(pid)] = struct{}{}
		}
	}
	ordered := make([]uint32, 0, len(pids))
	for pid := range pids {
		ordered = append(ordered, pid)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	clear(params.PIDTbl[:])
	params.PIDTblCount = uint32(copy(params.PIDTbl[:], ordered))
	if _, err := params.CopyOut(fi.t, addrFromP64(control.Params)); err != nil {
		return 0, err
	}
	return 0, frontendFailWithStatus(fi, control, nvgpu.NV_OK)
}

func ctrlGpuGetPidInfo(fi *frontendIoctlState, control *nvgpu.NVOS54_PARAMETERS) (uintptr, error) {
	var params nvgpu.NV2080_CTRL_GPU_GET_PID_INFO_PARAMS
	if control.ParamsSize != uint32(params.SizeBytes()) {
		return 0, linuxerr.EINVAL
	}
	if control.Flags != 0 {
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_INVALID_ARGUMENT)
	}
	if _, err := params.CopyIn(fi.t, addrFromP64(control.Params)); err != nil {
		return 0, err
	}
	if params.PIDInfoListCount == 0 || params.PIDInfoListCount > nvgpu.NV2080_CTRL_GPU_GET_PID_INFO_MAX_COUNT {
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_INVALID_ARGUMENT)
	}
	scope, status, err := processScope(fi, control)
	if err != nil {
		return 0, err
	}
	if status != nvgpu.NV_OK {
		return 0, frontendFailWithStatus(fi, control, status)
	}
	snapshots, ok := fi.fd.dev.nvp.snapshotProcesses(scope)
	if !ok {
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_NOT_SUPPORTED)
	}
	usage, ok := resolveProcessMemory(fi.t.PIDNamespace(), snapshots)
	if !ok {
		return 0, frontendFailWithStatus(fi, control, nvgpu.NV_ERR_NOT_SUPPORTED)
	}
	for index := range params.PIDInfoListCount {
		entry := &params.PIDInfoList[index]
		entry.SetVideoMemoryUsage(nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA{})
		entry.SMCSubscription = nvgpu.NV2080_CTRL_SMC_SUBSCRIPTION_INFO{ComputeInstanceID: math.MaxUint32, GPUInstanceID: math.MaxUint32}
		entry.Result = nvgpu.NV_OK
		if entry.Index != nvgpu.NV2080_CTRL_GPU_PID_INFO_INDEX_VIDEO_MEMORY_USAGE || entry.PID == 0 {
			entry.Result = nvgpu.NV_ERR_INVALID_ARGUMENT
			continue
		}
		entry.SetVideoMemoryUsage(usage[entry.PID])
	}
	if _, err := params.CopyOut(fi.t, addrFromP64(control.Params)); err != nil {
		return 0, err
	}
	return 0, frontendFailWithStatus(fi, control, nvgpu.NV_OK)
}
