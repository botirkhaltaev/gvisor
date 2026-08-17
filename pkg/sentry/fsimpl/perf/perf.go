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

// Package perf implements perf event file descriptors.
package perf

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// FileDescription implements vfs.FileDescriptionImpl for perf events.
//
// +stateify savable
type FileDescription struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	ev *kernel.PerfEvent
}

var _ vfs.FileDescriptionImpl = (*FileDescription)(nil)

// New returns a new perf event fd.
func New(ctx context.Context, vfsObj *vfs.VirtualFilesystem, ev *kernel.PerfEvent, flags uint32) (*vfs.FileDescription, error) {
	vd := vfsObj.NewAnonVirtualDentry("[perf_event]")
	defer vd.DecRef(ctx)
	pfd := &FileDescription{ev: ev}
	if err := pfd.vfsfd.Init(pfd, flags, auth.CredentialsFromContext(ctx), vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		DenyPRead:         true,
		DenyPWrite:        true,
	}); err != nil {
		return nil, err
	}
	return &pfd.vfsfd, nil
}

// Read implements vfs.FileDescriptionImpl.Read.
func (fd *FileDescription) Read(ctx context.Context, dst usermem.IOSequence, _ vfs.ReadOptions) (int64, error) {
	buf := make([]byte, dst.NumBytes())
	n, err := fd.ev.ReadFormat(buf)
	if err != nil {
		return 0, err
	}
	written, err := dst.CopyOut(ctx, buf[:n])
	if err != nil {
		return 0, err
	}
	return int64(written), nil
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *FileDescription) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	cmd := uint32(args[1].Uint())
	switch cmd {
	case linux.PERF_EVENT_IOC_ENABLE, linux.PERF_EVENT_IOC_DISABLE, linux.PERF_EVENT_IOC_RESET:
		if cmd == linux.PERF_EVENT_IOC_ENABLE {
			fd.ev.Enable()
		} else if cmd == linux.PERF_EVENT_IOC_DISABLE {
			fd.ev.Disable()
		} else {
			fd.ev.Reset()
		}
		return 0, nil
	case linux.PERF_EVENT_IOC_PERIOD:
		var period uint64
		if _, err := primitive.CopyUint64In(kernel.TaskFromContext(ctx), args[2].Pointer(), &period); err != nil {
			return 0, err
		}
		fd.ev.SetPeriod(period)
		return 0, nil
	case linux.PERF_EVENT_IOC_ID:
		if _, err := primitive.CopyUint64Out(kernel.TaskFromContext(ctx), args[2].Pointer(), fd.ev.ID()); err != nil {
			return 0, err
		}
		return 0, nil
	default:
		return 0, linuxerr.ENOTTY
	}
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *FileDescription) Release(ctx context.Context) {
	fd.ev.Disable()
	fd.ev.Target().RemovePerfEvent(fd.ev)
}
