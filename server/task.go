package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari-agent/dnsresolver"
	v2 "github.com/komari-monitor/komari-agent/protocol/v2"
	"github.com/komari-monitor/komari-agent/ws"
	ping "github.com/prometheus-community/pro-bing"
)

func NewTask(task_id, command string, timeoutSec int) {
	if task_id == "" {
		return
	}
	if strings.TrimSpace(command) == "" {
		uploadTaskResult(task_id, "No command provided", 0, time.Now())
		return
	}
	if flags.DisableWebSsh {
		uploadTaskResult(task_id, "Remote control is disabled.", -1, time.Now())
		return
	}
	log.Printf("Executing task %s with command: %s", task_id, command)
	result, exitCode := runTaskCommand(task_id, command, timeoutSec)
	uploadTaskResult(task_id, result, exitCode, time.Now())
}

func runTaskCommand(taskID, command string, timeoutSec int) (string, int) {
	// 单条命令超时：未指定(<=0)则用 agent 默认 flags.TaskExecTimeout；最终为 0 表示不限制。
	// 超时会连同子进程组一起被杀（见 setupProcessGroup），避免 ping/top 等挂死、留孤儿进程。
	if timeoutSec <= 0 {
		timeoutSec = flags.TaskExecTimeout
	}
	timeout := time.Duration(timeoutSec) * time.Second
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd, cleanup, err := buildTaskCommand(ctx, command)
	if err != nil {
		return err.Error(), -1
	}
	defer cleanup()

	// stdout/stderr 合并进一个并发安全缓冲，便于运行中增量上报「近实时」输出。
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		return err.Error(), -1
	}

	// 运行期间每 ~2s 把已产生的输出增量上报（仅在有变化时），前端轮询即可看到滚动增长。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	if taskID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			lastLen := -1
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					s := out.String()
					if len(s) != lastLen {
						lastLen = len(s)
						uploadTaskProgress(taskID, normalizeOutput(s))
					}
				}
			}
		}()
	}

	err = cmd.Wait()
	close(stop)
	wg.Wait()

	result := normalizeOutput(out.String())
	exitCode := 0
	switch {
	case timeout > 0 && ctx.Err() == context.DeadlineExceeded:
		// 超时被终止：保留已产生的输出，并补一行说明 + 约定的 124 退出码
		result = appendErrorResult(result, fmt.Sprintf("[komari] command timed out after %s and was terminated", timeout))
		exitCode = 124
	case err != nil:
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			result = appendErrorResult(result, err.Error())
			exitCode = -1
		}
	}

	return result, exitCode
}

func normalizeOutput(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// lockedBuffer 是并发安全的输出缓冲：exec 的拷贝 goroutine 写、上报 goroutine 读。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func buildTaskCommand(ctx context.Context, command string) (*exec.Cmd, func(), error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		scriptFile, err := os.CreateTemp("", "komari-task-*.ps1")
		if err != nil {
			return nil, func() {}, err
		}
		cleanup := func() {
			_ = os.Remove(scriptFile.Name())
		}
		if _, err := scriptFile.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			_ = scriptFile.Close()
			cleanup()
			return nil, func() {}, err
		}
		script := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\n" + command
		if _, err := scriptFile.WriteString(script); err != nil {
			_ = scriptFile.Close()
			cleanup()
			return nil, func() {}, err
		}
		if err := scriptFile.Close(); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptFile.Name())
		setupProcessGroup(cmd)
		return cmd, cleanup, nil
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-s")
		cmd.Stdin = strings.NewReader(command)
	}
	setupProcessGroup(cmd)
	return cmd, func() {}, nil
}

func appendErrorResult(result, err string) string {
	if result == "" {
		return err
	}
	return result + "\n" + err
}

// uploadTaskResult 上报最终结果（带 exit_code 与 finished_at，标记任务完成）。
func uploadTaskResult(taskID, result string, exitCode int, finishedAt time.Time) {
	postTaskResult(map[string]interface{}{
		"task_id":     taskID,
		"result":      result,
		"exit_code":   exitCode,
		"finished_at": finishedAt,
	}, flags.MaxRetries)
}

// uploadTaskProgress 上报进行中的增量输出（不带 exit_code / finished_at，服务端视为未完成）。
// 尽力而为，不重试——下一个 tick 会再次上报最新输出。
func uploadTaskProgress(taskID, result string) {
	postTaskResult(map[string]interface{}{
		"task_id": taskID,
		"result":  result,
	}, 0)
}

func postTaskResult(payload map[string]interface{}, maxRetry int) {
	jsonData, _ := json.Marshal(payload)
	endpoint := flags.Endpoint + "/api/clients/task/result?token=" + flags.Token

	client := dnsresolver.GetHTTPClient(30 * time.Second)
	for attempt := 0; attempt <= maxRetry; attempt++ {
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader(jsonData))
		if err != nil {
			log.Printf("Failed to create task result request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if flags.CFAccessClientID != "" && flags.CFAccessClientSecret != "" {
			req.Header.Set("CF-Access-Client-Id", flags.CFAccessClientID)
			req.Header.Set("CF-Access-Client-Secret", flags.CFAccessClientSecret)
		}

		resp, err := client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			return
		}
		if attempt == maxRetry {
			if err != nil {
				log.Printf("Failed to upload task result: %v", err)
			} else if resp != nil {
				log.Printf("Failed to upload task result: %s", resp.Status)
			}
			return
		}
		log.Printf("Failed to upload task result, retrying %d/%d", attempt+1, maxRetry)
		time.Sleep(2 * time.Second)
	}
}

// resolveIP 解析域名到 IP 地址，排除 DNS 查询时间
func resolveIP(target string) (string, error) {
	// 如果已经是 IP 地址，直接返回
	if ip := net.ParseIP(target); ip != nil {
		return target, nil
	}
	// 解析域名到 IP
	addrs, err := net.LookupHost(target)
	if err != nil || len(addrs) == 0 {
		return "", errors.New("failed to resolve target")
	}
	return addrs[0], nil // 返回第一个解析的 IP
}

func icmpPing(target string, timeout time.Duration) (int64, error) {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	// For ICMP, we only need the host/IP, port is irrelevant.
	// If the host is an IPv6 literal, it might be wrapped in brackets.
	host = strings.Trim(host, "[]")

	// 先解析 IP 地址
	ip, err := resolveIP(host)
	if err != nil {
		return -1, err
	}

	pinger, err := ping.NewPinger(ip)
	if err != nil {
		return -1, err
	}
	pinger.Count = 1
	pinger.Timeout = timeout
	pinger.SetPrivileged(true)
	err = pinger.Run()
	if err != nil {
		return -1, err
	}
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return -1, errors.New("no packets received")
	}
	return stats.AvgRtt.Milliseconds(), nil
}

func tcpPing(target string, timeout time.Duration) (int64, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		// No port, assume port 80
		host = target
		port = "80"
	}

	// If the host is an IPv6 literal, it might be wrapped in brackets.
	host = strings.Trim(host, "[]")

	ip, err := resolveIP(host)
	if err != nil {
		return -1, err
	}

	targetAddr := net.JoinHostPort(ip, port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", targetAddr, timeout)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	return time.Since(start).Milliseconds(), nil
}

func httpPing(target string, timeout time.Duration) (int64, error) {
	// Handle raw IPv6 address for URL
	if strings.Contains(target, ":") && !strings.Contains(target, "[") {
		// check if it's a valid IP to avoid wrapping hostnames
		if ip := net.ParseIP(target); ip != nil && ip.To4() == nil {
			target = "[" + target + "]"
		}
	}

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// 在 Dial 之前解析 IP，排除 DNS 时间
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ip, err := resolveIP(host)
				if err != nil {
					return nil, err
				}
				return net.DialTimeout(network, net.JoinHostPort(ip, port), timeout)
			},
		},
	}
	start := time.Now()
	resp, err := client.Get(target)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return latency, nil
	}
	return latency, errors.New("http status not ok")
}

func NewPingTask(conn *ws.SafeConn, protocolVersion int, taskID uint, pingType, pingTarget string) {
	if taskID == 0 {
		log.Printf("Invalid task ID: %d", taskID)
		return
	}
	var err error = nil
	var latency int64
	pingResult := -1
	timeout := 3 * time.Second           // 默认超时时间
	const highLatencyThreshold = 1000    // ms 阈值
	const retryDropThresholdTcping = 800 // ms 重试中延迟降低超过此值则基本认为发生重传
	// 800ms = SYN/SYN-ACK 首次超时重传 1000ms - 防误判容许 200ms 延迟抖动

	measure := func() (int64, error) {
		switch pingType {
		case "icmp":
			return icmpPing(pingTarget, timeout)
		case "tcp":
			return tcpPing(pingTarget, timeout)
		case "http":
			return httpPing(pingTarget, timeout)
		default:
			return -1, errors.New("unsupported ping type")
		}
	}
	PingHighLatencyRetries := 3
	// 首次测量
	if latency, err = measure(); err == nil {
		firstLatency := latency
		if latency > int64(highLatencyThreshold) && PingHighLatencyRetries > 0 {
			attempts := PingHighLatencyRetries
			for i := 0; i < attempts; i++ {
				if second, err2 := measure(); err2 == nil {
					if second <= int64(highLatencyThreshold) {
						if pingType == "tcp" && firstLatency-second > int64(retryDropThresholdTcping) {
							err = errors.New("suspicious retransmission detected in tcp handshake")
							break
						}
						latency = second
						break
					}
					if i == attempts-1 { // 最后一次仍高
						err = errors.New("latency remains high after retries")
					}
				} else {
					err = err2
					break
				}
			}
		}
	}

	if err != nil {
		log.Printf("Ping task %d failed: %v", taskID, err)
		pingResult = -1 // 如果有错误，设置结果为 -1
	} else {
		pingResult = int(latency)
	}
	finishedAt := time.Now()
	payload := map[string]interface{}{
		"type":        "ping_result",
		"task_id":     taskID,
		"ping_type":   pingType,
		"value":       pingResult,
		"finished_at": finishedAt,
	}
	var wsPayload interface{} = payload
	if protocolVersion >= 2 {
		wsPayload = v2.BuildPingResultPayload(taskID, pingType, pingResult, finishedAt)
	}
	// https://github.com/komari-monitor/komari/commit/eb87a4fc330b7d1c407fa4ff70177615a4f50a1f
	// -1 代表丢包，服务端计算
	//if pingResult == -1 {
	//	return
	//}
	if conn == nil {
		if protocolVersion >= 2 {
			if err := postV2RPC(wsPayload); err != nil {
				log.Printf("Failed to upload ping result over POST: %v", err)
			}
		}
		return
	}
	if err := conn.WriteJSON(wsPayload); err != nil {
		log.Printf("Failed to write JSON to WebSocket: %v", err)
	}

}

func postV2RPC(payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(flags.Endpoint, "/") + "/api/clients/v2/rpc?token=" + flags.Token
	compressed := false
	if !flags.DisableCompression {
		if gz, err := gzipBytes(body); err == nil {
			body = gz
			compressed = true
		}
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if compressed {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if flags.CFAccessClientID != "" && flags.CFAccessClientSecret != "" {
		req.Header.Set("CF-Access-Client-Id", flags.CFAccessClientID)
		req.Header.Set("CF-Access-Client-Secret", flags.CFAccessClientSecret)
	}
	client := dnsresolver.GetHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(body)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
