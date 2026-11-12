//go:build windows

package launcher

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW        = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob      = kernel32.NewProc("AssignProcessToJobObject")

	jobOnce   sync.Once
	jobHandle syscall.Handle
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	BasicLimitInformation jobBasicLimit
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// processJob returns a job object that kills its processes when this
// process exits and the handle is closed by the OS.
func processJob() syscall.Handle {
	jobOnce.Do(func() {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		info := jobExtendedLimit{}
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		ok, _, _ := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if ok == 0 {
			_ = syscall.CloseHandle(syscall.Handle(h))
			return
		}
		jobHandle = syscall.Handle(h)
	})
	return jobHandle
}

// afterStart puts the browser in a kill-on-close job, so it does not outlive
// this process even when this process is killed.
func afterStart(cmd *exec.Cmd) (stop func()) {
	stop = func() {}
	job := processJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	ph, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer func() { _ = syscall.CloseHandle(ph) }()
	_, _, _ = procAssignProcessToJob.Call(uintptr(job), uintptr(ph))
	return
}
