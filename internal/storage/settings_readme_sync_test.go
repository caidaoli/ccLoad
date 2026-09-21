//go:build sonic

// 本文件单独存在：现有 storage 测试按迁移主题和存储行为组织，没有覆盖
// “用户可见设置契约 ↔ README 文档”这一同步关系的文件。
package storage

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 设置表格的表头，中英文 README 各一种。
var readmeSettingHeader = regexp.MustCompile(`^\| (Setting|配置项) \|`)

// 设置表格的数据行，首列是反引号包裹的配置键。
var readmeSettingRow = regexp.MustCompile("^\\| `([^`]+)`")

// readmeSettingKeys 提取 README 第一张设置表格里列出的配置键。
// 扫完第一张表即停，避免后续同表头的渠道字段表被当成系统设置。
func readmeSettingKeys(t *testing.T, name string) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	keys := make(map[string]struct{})
	inTable := false
lines:
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case readmeSettingHeader.MatchString(line):
			if inTable {
				break lines
			}
			inTable = true
		case !inTable:
			// 表格之外的内容忽略。
		case !strings.HasPrefix(line, "|"):
			break lines
		default:
			if m := readmeSettingRow.FindStringSubmatch(line); m != nil {
				keys[m[1]] = struct{}{}
			}
		}
	}
	if len(keys) == 0 {
		t.Fatalf("%s: 未找到设置表格，表头格式可能已变更", name)
	}
	return keys
}

// missingKeys 返回在 want 中但不在 got 中的键，排序后便于阅读。
func missingKeys(want, got map[string]struct{}) []string {
	var out []string
	for k := range want {
		if _, ok := got[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestReadmeCoversEverySystemSetting 保证中英文 README 的设置表格与数据库里的
// 系统设置键集完全一致：新增设置项必须同步文档，删除设置项必须清理文档。
//
// 权威来源是跑完整迁移后 system_settings 表里的真实键，而不是源码文本——
// initDefaultSettings 写入、cleanupRemovedSettings 删除的效果都已经反映在其中，
// 因此这里不需要同时维护一份“已废弃键”名单。
func TestReadmeCoversEverySystemSetting(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT "+quoteKeyIdent(DialectSQLite)+" FROM system_settings")
	if err != nil {
		t.Fatalf("query system_settings: %v", err)
	}
	defer func() { _ = rows.Close() }()

	live := make(map[string]struct{})
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan setting key: %v", err)
		}
		live[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate system_settings: %v", err)
	}
	if len(live) == 0 {
		t.Fatal("system_settings 为空，initDefaultSettings 未生效")
	}

	for _, name := range []string{"README.md", "README.zh-CN.md"} {
		documented := readmeSettingKeys(t, name)
		if missing := missingKeys(live, documented); len(missing) > 0 {
			t.Errorf("%s 设置表格缺少 %d 个配置项，新增设置项时请同步文档: %v",
				name, len(missing), missing)
		}
		if stale := missingKeys(documented, live); len(stale) > 0 {
			t.Errorf("%s 设置表格残留 %d 个数据库中已不存在的配置项: %v",
				name, len(stale), stale)
		}
	}
}
