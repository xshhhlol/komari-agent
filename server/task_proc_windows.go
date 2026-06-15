//go:build windows

package server

import "os/exec"

// Windows 下 exec.CommandContext 在超时时会终止 powershell 进程；
// 子进程树的清理为尽力而为（如需严格清理可后续引入 Job Object）。
func setupProcessGroup(cmd *exec.Cmd) {}
