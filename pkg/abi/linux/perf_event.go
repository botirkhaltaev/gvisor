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

package linux

import (
	"structs"
)

// perf_event_attr.type values from include/uapi/linux/perf_event.h.
const (
	PERF_TYPE_HARDWARE   = 0
	PERF_TYPE_SOFTWARE   = 1
	PERF_TYPE_TRACEPOINT = 2
	PERF_TYPE_HW_CACHE   = 3
	PERF_TYPE_RAW        = 4
	PERF_TYPE_BREAKPOINT = 5
)

// Software event IDs for attr.config when type is PERF_TYPE_SOFTWARE.
const (
	PERF_COUNT_SW_CPU_CLOCK  = 0
	PERF_COUNT_SW_TASK_CLOCK = 1
)

// perf_event_attr.size ABI versions.
const (
	PERF_ATTR_SIZE_VER0 = 64
	PERF_ATTR_SIZE_VER8 = 136
)

// attr.sample_type bits.
const (
	PERF_SAMPLE_BRANCH_STACK = 1 << 11
)

// attr.read_format bits.
const (
	PERF_FORMAT_TOTAL_TIME_ENABLED = 1 << 0
	PERF_FORMAT_TOTAL_TIME_RUNNING = 1 << 1
	PERF_FORMAT_ID                 = 1 << 2
	PERF_FORMAT_GROUP              = 1 << 3
	PERF_FORMAT_LOST               = 1 << 4
)

// Packed attr bitfield (include/uapi/linux/perf_event.h).
const (
	PerfBitDisabled  = 1 << 0
	PerfBitInherit   = 1 << 1
	PerfBitAuxOutput = 1 << 31
)

// perf_event_open(2) flags.
const (
	PERF_FLAG_FD_NO_GROUP = 1 << 0
	PERF_FLAG_FD_OUTPUT   = 1 << 1
	PERF_FLAG_PID_CGROUP  = 1 << 2
	PERF_FLAG_FD_CLOEXEC  = 1 << 3
)

// ioctl(2) arg flag for ENABLE/DISABLE/RESET.
const (
	PERF_IOC_FLAG_GROUP = 1 << 0
)

// Ioctls from include/uapi/linux/perf_event.h (_IO('$', n)).
var (
	PERF_EVENT_IOC_ENABLE  = IO('$', 0)
	PERF_EVENT_IOC_DISABLE = IO('$', 1)
	PERF_EVENT_IOC_REFRESH = IO('$', 2)
	PERF_EVENT_IOC_RESET   = IO('$', 3)
	PERF_EVENT_IOC_PERIOD  = IOW('$', 4, 8)
	PERF_EVENT_IOC_ID      = IOR('$', 7, 8)
)

// PerfEventAttr is struct perf_event_attr through VER8.
//
// The Linux bitfield is packed into Bits.
//
// +marshal
type PerfEventAttr struct {
	_                structs.HostLayout
	Type             uint32
	Size             uint32
	Config           uint64
	SamplePeriod     uint64
	SampleType       uint64
	ReadFormat       uint64
	Bits             uint64
	WakeupEvents     uint32
	BPType           uint32
	BPAddr           uint64
	BPLen            uint64
	BranchSampleType uint64
	SampleRegsUser   uint64
	SampleStackUser  uint32
	ClockID          int32
	SampleRegsIntr   uint64
	AuxWatermark     uint32
	SampleMaxStack   uint16
	Reserved2        uint16
	AuxSampleSize    uint32
	AuxAction        uint32
	SigData          uint64
	Config3          uint64
}
