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
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/perf"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

func copyInPerfEventAttr(t *kernel.Task, addr hostarch.Addr) (linux.PerfEventAttr, error) {
	var size uint32
	if _, err := primitive.CopyUint32In(t, addr+4, &size); err != nil {
		return linux.PerfEventAttr{}, err
	}
	if size == 0 {
		size = linux.PERF_ATTR_SIZE_VER0
	}
	if size < linux.PERF_ATTR_SIZE_VER0 {
		return linux.PerfEventAttr{}, linuxerr.EINVAL
	}
	if size > hostarch.PageSize {
		return linux.PerfEventAttr{}, linuxerr.E2BIG
	}
	var attr linux.PerfEventAttr
	n := int(size)
	if n > attr.SizeBytes() {
		n = attr.SizeBytes()
	}
	if _, err := attr.CopyInN(t, addr, n); err != nil {
		return linux.PerfEventAttr{}, err
	}
	attr.Size = size
	if int(size) > attr.SizeBytes() {
		buf := make([]byte, int(size)-attr.SizeBytes())
		if _, err := t.CopyInBytes(addr+hostarch.Addr(attr.SizeBytes()), buf); err != nil {
			return linux.PerfEventAttr{}, err
		}
		for _, b := range buf {
			if b != 0 {
				return linux.PerfEventAttr{}, linuxerr.E2BIG
			}
		}
	}
	return attr, nil
}

// PerfEventOpen implements linux syscall perf_event_open(2).
func PerfEventOpen(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	attr, err := copyInPerfEventAttr(t, args[0].Pointer())
	if err != nil {
		return 0, nil, err
	}
	pid := args[1].Int()
	cpu := args[2].Int()
	groupFD := args[3].Int()
	flags := args[4].Uint64()

	if flags&^linux.PERF_FLAG_FD_CLOEXEC != 0 {
		return 0, nil, linuxerr.EINVAL
	}
	if groupFD != -1 {
		return 0, nil, linuxerr.EINVAL
	}
	if pid == -1 && cpu == -1 {
		return 0, nil, linuxerr.EINVAL
	}
	if pid == -1 {
		return 0, nil, linuxerr.EACCES
	}
	if cpu != -1 && (cpu < 0 || uint(cpu) >= t.Kernel().ApplicationCores()) {
		return 0, nil, linuxerr.EINVAL
	}

	target := t
	if pid > 0 {
		target = t.PIDNamespace().TaskWithID(kernel.ThreadID(pid))
		if target == nil {
			return 0, nil, linuxerr.ESRCH
		}
		if !t.CanTrace(target, false) {
			return 0, nil, linuxerr.EPERM
		}
	}

	if attr.SampleType&linux.PERF_SAMPLE_BRANCH_STACK != 0 || attr.Bits&linux.PerfBitAuxOutput != 0 {
		return 0, nil, linuxerr.EOPNOTSUPP
	}

	switch attr.Type {
	case linux.PERF_TYPE_HARDWARE, linux.PERF_TYPE_HW_CACHE, linux.PERF_TYPE_RAW:
		return 0, nil, linuxerr.ENOENT
	case linux.PERF_TYPE_SOFTWARE:
		if attr.Config != linux.PERF_COUNT_SW_CPU_CLOCK {
			return 0, nil, linuxerr.EOPNOTSUPP
		}
	default:
		return 0, nil, linuxerr.ENOENT
	}

	ev := kernel.NewPerfEvent(target, attr)
	file, err := perf.New(t, t.Kernel().VFS(), ev, linux.O_RDWR)
	if err != nil {
		target.RemovePerfEvent(ev)
		return 0, nil, err
	}
	defer file.DecRef(t)
	fd, err := t.NewFDFrom(0, file, kernel.FDFlags{
		CloseOnExec: flags&linux.PERF_FLAG_FD_CLOEXEC != 0,
	})
	if err != nil {
		return 0, nil, err
	}
	return uintptr(fd), nil, nil
}
