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
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
)

// processMemorySize queries only the two size selectors on an object that the
// driver has just allocated in this client. It does not request physical
// addresses or inspect any other client's resources.
// fd is the current ioctl's descriptor, pinned by the syscall's VFS reference.
// A saved client-creation descriptor is not sufficient: its host FD can close
// before client object teardown acquires objsMu.
func processMemorySize(fd *frontendFD, client, object nvgpu.Handle) (uint64, bool) {
	// Graphics-only legacy allocations do not arm RM_CONTROL in seccomp.
	// A telemetry query must not broaden that policy or kill the sentry.
	if fd == nil || fd.dev.nvp.capsEnabled&compUtil == 0 {
		return 0, false
	}
	info := [2]nvgpu.NVXXXX_CTRL_XXX_INFO{
		{Index: nvgpu.NV0041_CTRL_SURFACE_INFO_INDEX_PHYS_SIZE_LO},
		{Index: nvgpu.NV0041_CTRL_SURFACE_INFO_INDEX_PHYS_SIZE_HI},
	}
	params := nvgpu.NvxxxCtrlXxxGetInfoParams{
		InfoListSize: uint32(len(info)),
		InfoList:     p64FromPtr(unsafe.Pointer(&info[0])),
	}
	control := nvgpu.NVOS54_PARAMETERS{
		HClient:    client,
		HObject:    object,
		Cmd:        nvgpu.NV0041_CTRL_CMD_GET_SURFACE_INFO,
		Params:     p64FromPtr(unsafe.Pointer(&params)),
		ParamsSize: uint32(params.SizeBytes()),
	}
	_, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), frontendIoctlCmd(nvgpu.NV_ESC_RM_CONTROL, nvgpu.SizeofNVOS54Parameters), uintptr(unsafe.Pointer(&control)))
	runtime.KeepAlive(&info)
	runtime.KeepAlive(&params)
	runtime.KeepAlive(fd)
	if errno != 0 || control.Status != nvgpu.NV_OK {
		return 0, false
	}
	return uint64(info[0].Data) | uint64(info[1].Data)<<32, true
}

func processOwnMemory(fd *frontendFD, client, object nvgpu.Handle) (nvgpu.NV2080_CTRL_GPU_PID_INFO, uint32, error) {
	params := nvgpu.NV2080_CTRL_GPU_GET_PID_INFO_PARAMS{PIDInfoListCount: 1}
	params.PIDInfoList[0].PID = uint32(unix.Getpid())
	control := nvgpu.NVOS54_PARAMETERS{
		HClient:    client,
		HObject:    object,
		Cmd:        nvgpu.NV2080_CTRL_CMD_GPU_GET_PID_INFO,
		Params:     p64FromPtr(unsafe.Pointer(&params)),
		ParamsSize: uint32(params.SizeBytes()),
	}
	_, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), frontendIoctlCmd(nvgpu.NV_ESC_RM_CONTROL, nvgpu.SizeofNVOS54Parameters), uintptr(unsafe.Pointer(&control)))
	runtime.KeepAlive(&params)
	runtime.KeepAlive(fd)
	if errno != 0 {
		return params.PIDInfoList[0], control.Status, errno
	}
	if control.Status != nvgpu.NV_OK {
		return params.PIDInfoList[0], control.Status, nil
	}
	return params.PIDInfoList[0], params.PIDInfoList[0].Result, nil
}

func processOwnPidHasClass(fd *frontendFD, client, object nvgpu.Handle, class uint32) (bool, uint32, error) {
	params := nvgpu.NV2080_CTRL_GPU_GET_PIDS_PARAMS{IDType: nvgpu.NV2080_CTRL_GPU_GET_PIDS_ID_TYPE_CLASS, ID: class}
	control := nvgpu.NVOS54_PARAMETERS{
		HClient:    client,
		HObject:    object,
		Cmd:        nvgpu.NV2080_CTRL_CMD_GPU_GET_PIDS,
		Params:     p64FromPtr(unsafe.Pointer(&params)),
		ParamsSize: uint32(params.SizeBytes()),
	}
	_, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), frontendIoctlCmd(nvgpu.NV_ESC_RM_CONTROL, nvgpu.SizeofNVOS54Parameters), uintptr(unsafe.Pointer(&control)))
	runtime.KeepAlive(&params)
	runtime.KeepAlive(fd)
	if errno != 0 {
		return false, control.Status, errno
	}
	if control.Status != nvgpu.NV_OK {
		return false, control.Status, nil
	}
	if params.PIDTblCount > nvgpu.NV2080_CTRL_GPU_GET_PIDS_MAX_COUNT {
		return false, nvgpu.NV_ERR_NOT_SUPPORTED, nil
	}
	pid := uint32(unix.Getpid())
	for _, candidate := range params.PIDTbl[:params.PIDTblCount] {
		if candidate == pid {
			return true, nvgpu.NV_OK, nil
		}
	}
	return false, nvgpu.NV_OK, nil
}
