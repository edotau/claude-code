//go:build windows

package pi

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type stepTree struct{ job windows.Handle }

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// startStep starts cmd suspended and puts it in a kill-on-close job before it runs, so no child escapes the job.
func startStep(cmd *exec.Cmd) (*stepTree, error) {
	job, err := newKillJob()
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	cmd.Cancel = func() error { return windows.TerminateJobObject(job, 1) }
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if err := joinAndResume(job, uint32(cmd.Process.Pid)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // releases the process handle, pipes and context watcher
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &stepTree{job}, nil
}

func newKillJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// joinAndResume assigns the suspended process to job, then resumes it.
func joinAndResume(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return err
	}
	if r, _, _ := ntResumeProcess.Call(uintptr(h)); r != 0 { // an NTSTATUS: non-zero is a failure
		return syscall.Errno(r)
	}
	return nil
}

// reap kills everything the step left; closing the job also kills it should this process die first.
func (t *stepTree) reap() {
	_ = windows.TerminateJobObject(t.job, 1)
	_ = windows.CloseHandle(t.job)
}
