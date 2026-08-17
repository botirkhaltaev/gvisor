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

#include <errno.h>
#include <fcntl.h>
#include <linux/perf_event.h>
#include <sys/ioctl.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <cstdint>
#include <fstream>
#include <string>

#include "gtest/gtest.h"
#include "test/util/file_descriptor.h"
#include "test/util/save_util.h"
#include "test/util/test_util.h"

namespace gvisor {
namespace testing {

namespace {

int PerfEventOpen(struct perf_event_attr* attr, pid_t pid, int cpu, int group_fd,
                  unsigned long flags) {
  int fd = syscall(__NR_perf_event_open, attr, pid, cpu, group_fd, flags);
  MaybeSave();
  return fd;
}

struct perf_event_attr DefaultSwCpuClockAttr() {
  struct perf_event_attr attr = {};
  attr.size = sizeof(attr);
  attr.type = PERF_TYPE_SOFTWARE;
  attr.config = PERF_COUNT_SW_CPU_CLOCK;
  return attr;
}

TEST(PerfEventOpenTest, SwCpuClockCounting) {
  auto attr = DefaultSwCpuClockAttr();
  FileDescriptor fd(PerfEventOpen(&attr, 0, -1, -1, 0));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
}

TEST(PerfEventOpenTest, CloseOnExec) {
  auto attr = DefaultSwCpuClockAttr();
  FileDescriptor fd(
      PerfEventOpen(&attr, 0, -1, -1, PERF_FLAG_FD_CLOEXEC));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  int flags = fcntl(fd.get(), F_GETFD);
  ASSERT_THAT(flags, SyscallSucceeds());
  EXPECT_TRUE(flags & FD_CLOEXEC);
}

TEST(PerfEventOpenTest, PidMinusOneCpuMinusOneEinval) {
  auto attr = DefaultSwCpuClockAttr();
  EXPECT_THAT(PerfEventOpen(&attr, -1, -1, -1, 0),
              SyscallFailsWithErrno(EINVAL));
}

TEST(PerfEventOpenTest, PidMinusOneEacces) {
  auto attr = DefaultSwCpuClockAttr();
  EXPECT_THAT(PerfEventOpen(&attr, -1, 0, -1, 0),
              SyscallFailsWithErrno(EACCES));
}

TEST(PerfEventOpenTest, CpuTooLargeEinval) {
  auto attr = DefaultSwCpuClockAttr();
  EXPECT_THAT(PerfEventOpen(&attr, 0, 1 << 20, -1, 0),
              SyscallFailsWithErrno(EINVAL));
}

TEST(PerfEventOpenTest, HardwareEnoentOnGvisor) {
  SKIP_IF(!IsRunningOnGvisor());
  struct perf_event_attr attr = {};
  attr.size = sizeof(attr);
  attr.type = PERF_TYPE_HARDWARE;
  attr.config = PERF_COUNT_HW_CPU_CYCLES;
  EXPECT_THAT(PerfEventOpen(&attr, 0, -1, -1, 0),
              SyscallFailsWithErrno(ENOENT));
}

TEST(PerfEventOpenTest, RawEnoentOnGvisor) {
  SKIP_IF(!IsRunningOnGvisor());
  struct perf_event_attr attr = {};
  attr.size = sizeof(attr);
  attr.type = PERF_TYPE_RAW;
  EXPECT_THAT(PerfEventOpen(&attr, 0, -1, -1, 0),
              SyscallFailsWithErrno(ENOENT));
}

TEST(PerfEventOpenTest, TaskClockEopnotsupOnGvisor) {
  SKIP_IF(!IsRunningOnGvisor());
  struct perf_event_attr attr = {};
  attr.size = sizeof(attr);
  attr.type = PERF_TYPE_SOFTWARE;
  attr.config = PERF_COUNT_SW_TASK_CLOCK;
  EXPECT_THAT(PerfEventOpen(&attr, 0, -1, -1, 0),
              SyscallFailsWithErrno(EOPNOTSUPP));
}

TEST(PerfEventOpenTest, BranchStackEopnotsupOnGvisor) {
  SKIP_IF(!IsRunningOnGvisor());
  auto attr = DefaultSwCpuClockAttr();
  attr.sample_type = PERF_SAMPLE_BRANCH_STACK;
  EXPECT_THAT(PerfEventOpen(&attr, 0, -1, -1, 0),
              SyscallFailsWithErrno(EOPNOTSUPP));
}

TEST(PerfEventOpenTest, EnableDisableReadReset) {
  auto attr = DefaultSwCpuClockAttr();
  FileDescriptor fd(PerfEventOpen(&attr, 0, -1, -1, 0));
  ASSERT_THAT(fd.get(), SyscallSucceeds());

  ASSERT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_DISABLE, 0), SyscallSucceeds());
  ASSERT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_ENABLE, 0), SyscallSucceeds());
  for (volatile int i = 0; i < 1000000; ++i) {
  }
  ASSERT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_DISABLE, 0), SyscallSucceeds());

  uint64_t count = 0;
  ASSERT_THAT(read(fd.get(), &count, sizeof(count)),
              SyscallSucceedsWithValue(sizeof(count)));
  EXPECT_GT(count, 0u);

  ASSERT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_RESET, 0), SyscallSucceeds());
  count = 1;
  ASSERT_THAT(read(fd.get(), &count, sizeof(count)),
              SyscallSucceedsWithValue(sizeof(count)));
  EXPECT_EQ(count, 0u);

  uint64_t id = 0;
  ASSERT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_ID, &id), SyscallSucceeds());
  EXPECT_NE(id, 0u);
  uint64_t id2 = 0;
  ASSERT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_ID, &id2), SyscallSucceeds());
  EXPECT_EQ(id, id2);

  EXPECT_THAT(ioctl(fd.get(), PERF_EVENT_IOC_REFRESH, 0),
              SyscallFailsWithErrno(ENOTTY));
}

TEST(PerfEventOpenTest, ReadTooSmallEnospc) {
  auto attr = DefaultSwCpuClockAttr();
  FileDescriptor fd(PerfEventOpen(&attr, 0, -1, -1, 0));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  char buf[4];
  EXPECT_THAT(read(fd.get(), buf, sizeof(buf)), SyscallFailsWithErrno(ENOSPC));
}

TEST(PerfEventOpenTest, ParanoidOnGvisor) {
  SKIP_IF(!IsRunningOnGvisor());
  std::ifstream in("/proc/sys/kernel/perf_event_paranoid");
  ASSERT_TRUE(in.good());
  std::string value;
  in >> value;
  EXPECT_EQ(value, "2");
}

}  // namespace

}  // namespace testing
}  // namespace gvisor
