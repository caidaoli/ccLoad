//go:build mysql_integration || postgres_integration

package storage

import (
	"os/exec"
	"strings"
	"testing"
)

// lastNonEmptyLine 取 CombinedOutput 最后一行非空内容（容器 ID）。
func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

// dockerMappedHostPort 解析 `docker port` 输出中的宿主机端口。
func dockerMappedHostPort(t *testing.T, containerID, privatePort string) string {
	t.Helper()

	out, err := exec.Command("docker", "port", containerID, privatePort).CombinedOutput()
	if err != nil {
		t.Fatalf("获取容器端口映射失败: %v\n%s", err, out)
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		t.Fatalf("容器端口映射为空: container=%s port=%s", containerID[:12], privatePort)
	}

	// docker port 有时返回多行；我们只需要第一条映射
	line = strings.Split(line, "\n")[0]
	if strings.Contains(line, "->") {
		parts := strings.Split(line, "->")
		line = strings.TrimSpace(parts[len(parts)-1])
	}

	idx := strings.LastIndex(line, ":")
	if idx == -1 || idx == len(line)-1 {
		t.Fatalf("无法解析容器端口映射: %q", line)
	}

	return line[idx+1:]
}
