//go:build !windows

package terminal

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// terminfoSearchDirs 返回 terminfo 数据库可能所在的目录（与 ncurses 查找顺序一致）。
func terminfoSearchDirs() []string {
	dirs := []string{}
	if d := os.Getenv("TERMINFO"); d != "" {
		dirs = append(dirs, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".terminfo"))
	}
	if d := os.Getenv("TERMINFO_DIRS"); d != "" {
		for _, p := range strings.Split(d, ":") {
			if p != "" {
				dirs = append(dirs, p)
			}
		}
	}
	dirs = append(dirs,
		"/etc/terminfo",
		"/lib/terminfo",
		"/usr/share/terminfo",
		"/usr/lib/terminfo",
		"/usr/local/share/terminfo",
	)
	return dirs
}

// terminfoExists 判断给定 TERM 名称在本机是否有对应的 terminfo 条目。
// 条目通常位于 <dir>/<首字母>/<名称>，部分系统用十六进制目录 <dir>/<hex>/<名称>。
func terminfoExists(name string) bool {
	if name == "" {
		return false
	}
	first := name[:1]
	hexFirst := fmt.Sprintf("%x", name[0])
	for _, dir := range terminfoSearchDirs() {
		for _, sub := range []string{first, hexFirst} {
			if _, err := os.Stat(filepath.Join(dir, sub, name)); err == nil {
				return true
			}
		}
	}
	return false
}

// pickTerm 选择一个既支持 256 色、又不会触发 xterm 版本/能力探测级联的 TERM。
// 在 TERM=xterm* 下，vim 等程序启动时会发探测序列（如 XTGETTCAP）并等待终端回应，
// 而网页终端 xterm.js 不回应其中部分查询，导致 vim 卡死。screen/tmux 的 terminfo
// 没有这些探测能力，可规避卡死并保留 256 色。优先使用本机已安装的条目；都不存在时
// 回退到 xterm-256color（几乎必然存在，但此时可能仍需在被控机用 vimrc 禁用探测）。
func pickTerm() string {
	for _, candidate := range []string{"tmux-256color", "screen-256color"} {
		if terminfoExists(candidate) {
			return candidate
		}
	}
	return "xterm-256color"
}

// newTerminalImpl 创建一个新的终端实例。
// 它会尝试根据用户配置文件查找默认 shell，如果失败则回退到常见 shell。
// 优先以交互模式启动 shell，如果不支持则回退到非交互模式。
func newTerminalImpl() (*terminalImpl, error) {
	shell := ""
	// 从 /etc/passwd 获取用户默认 shell
	userHomeDir, err := os.UserHomeDir() // 获取当前用户的主目录
	if err == nil {
		passwdContent, err := os.ReadFile("/etc/passwd")
		if err == nil {
			for _, line := range strings.Split(string(passwdContent), "\n") {
				if strings.Contains(line, userHomeDir) {
					parts := strings.Split(line, ":")
					if len(parts) >= 7 && parts[6] != "" {
						shell = parts[6]
						//log.Printf("Found shell from /etc/passwd: %s for user home: %s\n", shell, userHomeDir)
						break
					}
				}
			}
		} else {
			log.Printf("Error reading /etc/passwd: %v\n", err)
		}
	} else {
		log.Printf("Error getting user home directory: %v\n", err)
	}

	// 验证从 /etc/passwd 获取的 shell 是否可用
	if shell != "" {
		if _, err := exec.LookPath(shell); err != nil {
			log.Printf("Shell '%s' from /etc/passwd not found in PATH, falling back.\n", shell)
			shell = "" // 默认 shell 不可用，清空以进入回退逻辑
		}
	}

	// 回退到默认 shell 列表
	defaultShells := []string{"zsh", "bash", "sh"}
	if shell == "" {
		log.Println("Shell not found or invalid, trying default shells.")
		for _, s := range defaultShells {
			if _, err := exec.LookPath(s); err == nil {
				shell = s
				log.Printf("Using default shell: %s\n", shell)
				break
			}
		}
	}

	if shell == "" {
		return nil, fmt.Errorf("no supported shell found among %v", defaultShells)
	}

	shellArgv0 := filepath.Base(shell)

	var prefixCmd string
	if shellArgv0 == "zsh" {
		prefixCmd = "unsetopt NOMATCH 2>/dev/null; "
	}

	shellCmd := prefixCmd + "for f in /etc/update-motd.d/*; do [ -e \"$f\" ] && [ -x \"$f\" ] && \"$f\"; done; [ -r /etc/motd ] && cat /etc/motd; exec \"$0\""

	// 选择不会触发 xterm 探测级联、又支持 256 色的 TERM，避免 vim 等全屏程序在网页终端里卡死。
	termEnv := pickTerm()

	cmd := exec.Command(shell, "-c", shellCmd)
	cmd.Args[0] = shellArgv0
	cmd.Env = append(os.Environ(), // 继承系统环境变量
		"TERM="+termEnv,  // 终端类型：优先 tmux/screen-256color，回退 xterm-256color
		"LANG=C.UTF-8",   // 设置语言环境为 UTF-8
		"LC_ALL=C.UTF-8", // 强制所有本地化变量为 UTF-8
	)

	tty, err := pty.Start(cmd)
	if err != nil {
		// 回退到原始启动逻辑（直接启动 shell，再无参数）
		cmd = exec.Command(shell)
		cmd.Env = append(os.Environ(),
			"TERM="+termEnv,
			"LANG=C.UTF-8",
			"LC_ALL=C.UTF-8",
		)
		tty, err = pty.Start(cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to start pty with argv0 prelude and plain shell: %v", err)
		}
	}

	// 设置初始终端大小
	pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80})

	return &terminalImpl{
		shell: shell,
		term: &unixTerminal{
			tty: tty,
			cmd: cmd,
		},
	}, nil
}

// unixTerminal 实现了 Unix 系统下的终端接口。
type unixTerminal struct {
	tty *os.File  // 伪终端设备文件
	cmd *exec.Cmd // 启动的 shell 进程命令
}

// Close 关闭终端，并尝试优雅地终止 shell 进程及其子进程。
func (t *unixTerminal) Close() error {
	if t.cmd == nil || t.cmd.Process == nil {
		return fmt.Errorf("terminal process is already nil or not started")
	}

	// 获取进程组 ID (PGID)。如果获取失败，则使用进程 PID 作为回退。
	// 向进程组发送信号可以确保 shell 启动的子进程也能接收到信号。
	pgid, err := syscall.Getpgid(t.cmd.Process.Pid)
	if err != nil {
		log.Printf("Failed to get process group ID for PID %d: %v. Using PID as PGID.\n", t.cmd.Process.Pid, err)
		pgid = t.cmd.Process.Pid
	}

	// 发送 SIGTERM 信号，请求进程组优雅退出
	log.Printf("Sending SIGTERM to process group %d...\n", pgid)
	_ = syscall.Kill(-pgid, syscall.SIGTERM) // -pgid 表示发送给进程组

	done := make(chan error, 1)
	go func() {
		// 等待命令退出。如果命令已经退出，Wait()会立即返回。
		done <- t.cmd.Wait()
	}()

	select {
	case err := <-done:
		// 进程已退出
		if err == nil {
			return nil
		}
		// 如果是 ExitError 且进程已退出，也视为成功关闭
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.Exited() {
			return nil // 进程已退出，尽管可能不是0状态码，但我们认为它已关闭
		}
		return fmt.Errorf("process group did not exit gracefully: %v", err)
	case <-time.After(5 * time.Second):
		// 5 秒内未退出，发送 SIGKILL 强制终止
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// 再次等待，确保进程被杀死，并获取最终的退出状态
		killErr := <-done
		if killErr == nil {
			return nil
		}
		if exitErr, ok := killErr.(*exec.ExitError); ok && exitErr.Exited() {
			return nil
		}
		log.Printf("Failed to kill process group %d after SIGKILL: %v\n", pgid, killErr)
		return fmt.Errorf("failed to kill process group %d: %v", pgid, killErr)
	}
}

// Read 从伪终端读取数据。
func (t *unixTerminal) Read(p []byte) (int, error) {
	if t.tty == nil {
		return 0, fmt.Errorf("tty is nil")
	}
	return t.tty.Read(p)
}

// Write 向伪终端写入数据。
func (t *unixTerminal) Write(p []byte) (int, error) {
	if t.tty == nil {
		return 0, fmt.Errorf("tty is nil")
	}
	return t.tty.Write(p)
}

// Resize 调整伪终端的大小。
func (t *unixTerminal) Resize(cols, rows int) error {
	if t.tty == nil {
		return fmt.Errorf("tty is nil")
	}
	return pty.Setsize(t.tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Wait 等待 shell 进程退出。
func (t *unixTerminal) Wait() error {
	if t.cmd == nil {
		return fmt.Errorf("command is nil")
	}
	return t.cmd.Wait()
}
