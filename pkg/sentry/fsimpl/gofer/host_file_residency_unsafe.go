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

package gofer

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// hostFilePagesResidentForRead is a snapshot; pages can be evicted before the
// read, so a resident result does not guarantee non-blocking host I/O.
func hostFilePagesResidentForRead(ctx context.Context, fd int, offset, length int64) bool {
	if offset < 0 || length <= 0 || length > 1<<30 {
		return false
	}
	ctx.UninterruptibleSleepStart()
	defer ctx.UninterruptibleSleepFinish()
	aligned := offset &^ int64(hostarch.PageSize-1)
	mapLength := int(length + (offset - aligned))
	mapping, err := unix.Mmap(fd, aligned, mapLength, unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return false
	}
	defer unix.Munmap(mapping)

	vec := make([]byte, (mapLength+hostarch.PageSize-1)/hostarch.PageSize)
	_, _, errno := unix.Syscall(unix.SYS_MINCORE,
		uintptr(unsafe.Pointer(&mapping[0])), uintptr(mapLength), uintptr(unsafe.Pointer(&vec[0])))
	runtime.KeepAlive(mapping)
	runtime.KeepAlive(vec)
	if errno != 0 {
		return false
	}
	for _, status := range vec {
		if status&1 == 0 {
			return false
		}
	}
	return true
}
