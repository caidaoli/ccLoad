//go:build mysql_integration || postgres_integration

package storage

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
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

func verifyFingerprintStorageContract(t *testing.T, primary Store) {
	t.Helper()

	ctx := context.Background()
	createdAt := time.Unix(1_700_000_000, 0)
	updatedAt := time.Unix(1_700_000_100, 0)
	newFingerprint := func(id int64, name string) *model.ModelFingerprint {
		return &model.ModelFingerprint{
			ID:            id,
			Name:          name,
			Model:         "gpt-database-integration",
			SampleCount:   3,
			Distribution:  []float64{0.5, 0.25, 0.25},
			Stats:         model.FingerprintStats{Mean: 2, Median: 2, Min: 1, Max: 3, Unique: 3, Mode: 1, ModeCount: 1},
			RawData:       []int{1, 2, 3},
			PromptVersion: "v1",
			CreatedAt:     model.JSONTime{Time: createdAt},
			UpdatedAt:     model.JSONTime{Time: updatedAt},
		}
	}

	explicit, err := primary.CreateModelFingerprint(ctx, newFingerprint(4200, "explicit"))
	if err != nil {
		t.Fatalf("CreateModelFingerprint explicit ID: %v", err)
	}
	if explicit.ID != 4200 || !explicit.CreatedAt.Equal(createdAt) || !explicit.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("explicit fingerprint not preserved: %#v", explicit)
	}
	automatic, err := primary.CreateModelFingerprint(ctx, newFingerprint(0, "automatic"))
	if err != nil {
		t.Fatalf("CreateModelFingerprint automatic ID: %v", err)
	}
	if automatic.ID <= explicit.ID {
		t.Fatalf("automatic fingerprint ID=%d, want > %d", automatic.ID, explicit.ID)
	}

	explicitResult := &model.FingerprintTestRecord{
		ID:          5200,
		Model:       "gpt-database-integration",
		SampleCount: 3,
		BestScore:   0.9,
		MatchesJSON: `[{"score":0.9}]`,
		CreatedAt:   model.JSONTime{Time: createdAt},
	}
	if err := primary.CreateFingerprintTestResult(ctx, explicitResult); err != nil {
		t.Fatalf("CreateFingerprintTestResult explicit ID: %v", err)
	}
	automaticResult := &model.FingerprintTestRecord{Model: "automatic", MatchesJSON: `[]`}
	if err := primary.CreateFingerprintTestResult(ctx, automaticResult); err != nil {
		t.Fatalf("CreateFingerprintTestResult automatic ID: %v", err)
	}
	if automaticResult.ID <= explicitResult.ID {
		t.Fatalf("automatic result ID=%d, want > %d", automaticResult.ID, explicitResult.ID)
	}

	primarySQL, ok := primary.(*sqlstore.SQLStore)
	if !ok {
		t.Fatalf("primary store type=%T, want *sqlstore.SQLStore", primary)
	}
	replica, err := CreateSQLiteStore(filepath.Join(t.TempDir(), "fingerprint-replica.db"))
	if err != nil {
		t.Fatalf("CreateSQLiteStore: %v", err)
	}
	defer func() { _ = replica.Close() }()
	replicaSQL, ok := replica.(*sqlstore.SQLStore)
	if !ok {
		t.Fatalf("replica store type=%T, want *sqlstore.SQLStore", replica)
	}
	if err := NewSyncManager(primarySQL, replicaSQL).RestoreOnStartup(ctx, 0); err != nil {
		t.Fatalf("RestoreOnStartup: %v", err)
	}

	restored, err := replica.GetModelFingerprint(ctx, explicit.ID)
	if err != nil {
		t.Fatalf("GetModelFingerprint restored ID=%d: %v", explicit.ID, err)
	}
	if restored.Name != explicit.Name || !restored.CreatedAt.Equal(createdAt) || !restored.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("restored fingerprint differs: %#v", restored)
	}
	restoredResults, err := replica.ListFingerprintTestResults(ctx, 10)
	if err != nil {
		t.Fatalf("ListFingerprintTestResults restored: %v", err)
	}
	foundExplicit := false
	foundAutomatic := false
	for _, result := range restoredResults {
		foundExplicit = foundExplicit || result.ID == explicitResult.ID
		foundAutomatic = foundAutomatic || result.ID == automaticResult.ID
	}
	if !foundExplicit || !foundAutomatic {
		t.Fatalf("restored result IDs missing: explicit=%v automatic=%v results=%#v", foundExplicit, foundAutomatic, restoredResults)
	}
}
