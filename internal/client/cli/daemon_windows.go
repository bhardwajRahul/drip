//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// getSysProcAttr returns platform-specific process attributes for daemonization
func getSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// isProcessRunningOS checks if a process is running using OS-specific method
func isProcessRunningOS(process *os.Process) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}

	return exitCode == 259
}

// killProcessOS kills a process using OS-specific method
func killProcessOS(process *os.Process) error {
	// On Windows, use Kill() directly
	return process.Kill()
}

// setupDaemonCmd configures the command for daemon mode
func setupDaemonCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = getSysProcAttr()
}
