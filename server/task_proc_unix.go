//go:build !windows

package server

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup 让命令在独立进程组中运行，并在 ctx 取消（超时）时
// 杀掉整个进程组——这样 `sh -c` 派生出的子进程（如 ping）也会一并终止，
// 不会变成孤儿继续占用资源。
func setupProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负 PID 表示向整个进程组发送信号
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
