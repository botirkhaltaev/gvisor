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

import "gvisor.dev/gvisor/pkg/abi/nvgpu"

// processMemory represents one Memory resource's contribution to process
// accounting. size is the driver's Memory.Length, not a requested allocation
// size. Each alias has its own record, with all aliases linked in the driver's
// duplication order. The records must include every alias of the allocation;
// an incomplete alias set cannot produce authoritative accounting.
//
// Concurrent access must be serialized by the owner of the records.
//
// +stateify savable
type processMemory struct {
	size      uint64
	protected bool
	owner     bool
	prev      *processMemory
	next      *processMemory
}

func newProcessMemory(size uint64, protected bool) *processMemory {
	m := &processMemory{size: size, protected: protected, owner: true}
	m.prev, m.next = m, m
	return m
}

// duplicate mirrors memCopyConstruct_IMPL(): the new resource is inserted
// immediately after the source resource and does not own the allocation.
// The source must be live.
func (m *processMemory) duplicate() *processMemory {
	if m.next == nil {
		panic("duplicating a released processMemory")
	}
	dup := &processMemory{
		size:      m.size,
		protected: m.protected,
		prev:      m,
		next:      m.next,
	}
	m.next.prev = dup
	m.next = dup
	return dup
}

// release mirrors _memDestructCommonWithDevice(): the successor of an owner
// inherits ownership before the resource is unlinked. A released resource
// contributes no memory to any process.
func (m *processMemory) release() {
	if m.next == nil {
		return
	}
	if m.owner {
		m.next.owner = true
	}
	m.prev.next = m.next
	m.next.prev = m.prev
	m.prev, m.next = nil, nil
	m.owner = false
}

// usage mirrors _gpuCollectMemInfo() for an eligible Memory resource. A
// protected allocation contributes to both the ordinary and protected totals.
func (m *processMemory) usage() nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA {
	var usage nvgpu.NV2080_CTRL_GPU_PID_INFO_VIDEO_MEMORY_USAGE_DATA
	if m.next == nil {
		return usage
	}
	if m.next == m {
		usage.MemPrivate = m.size
		if m.protected {
			usage.ProtectedMemPrivate = m.size
		}
	} else if m.owner {
		usage.MemSharedOwned = m.size
		if m.protected {
			usage.ProtectedMemSharedOwned = m.size
		}
	} else {
		usage.MemSharedDuped = m.size
		if m.protected {
			usage.ProtectedMemSharedDuped = m.size
		}
	}
	return usage
}
