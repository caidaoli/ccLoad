package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"ccLoad/internal/model"
)

/**
 * @brief 重开数据库迁移两站旧付费端点，保留凭证、精确模式和禁用状态，重复启动幂等。
 * @param t 测试上下文。
 */
func TestMigrate_SQLite_ZAIDirectURLs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "zai-urls.db")
	store, err := createSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	type sample struct {
		id       int64
		urls     model.ChannelURLs
		disabled []string
		auth     string
	}
	var samples []sample
	for _, site := range []struct{ old, next string }{
		{"https://zcode.z.ai/api/v1/ultra/anthropic", "https://open.bigmodel.cn/api/anthropic"},
		{"https://zcode.z.ai/api/v1/ultra-zai/anthropic", "https://api.z.ai/api/anthropic"},
	} {
		for _, auth := range []string{model.AuthTypeZAIOAuth, model.AuthTypeAPIKey} {
			urls := model.ChannelURLs{
				{URL: site.old, Protocols: []string{"anthropic"}},
				{URL: site.old + "/v1/messages", Exact: true, Protocols: []string{"anthropic"}},
				{URL: "https://custom.example.com/anthropic", Protocols: []string{"openai", "anthropic"}},
				{URL: "https://zcode.z.ai/api/v1/zcode-plan/anthropic"},
			}
			if auth == model.AuthTypeZAIOAuth {
				// 新旧地址同时登记时必须合并，不能因重复项阻止启动。
				urls = append(urls,
					model.ChannelURL{URL: site.next, Protocols: []string{"openai", "anthropic"}},
					model.ChannelURL{URL: site.next + "/v1/messages", Exact: true})
			}
			credential := ""
			if auth == model.AuthTypeZAIOAuth {
				credential = `{"api_key":"unchanged"}`
			}
			cfg, err := store.CreateConfig(ctx, &model.Config{Name: fmt.Sprintf("old-%d", len(samples)), AuthType: auth, OAuthCredential: credential, URLs: urls, Enabled: false})
			if err != nil {
				t.Fatal(err)
			}
			for _, u := range urls[:2] {
				if err := store.SetURLDisabled(ctx, cfg.ID, u.RuntimeURL(), true); err != nil {
					t.Fatal(err)
				}
			}
			// 已有目标状态也不能覆盖旧地址的手动禁用。
			if auth == model.AuthTypeZAIOAuth {
				if err := store.SetURLDisabled(ctx, cfg.ID, site.next, false); err != nil {
					t.Fatal(err)
				}
			}
			want := urls.Clone()
			if auth == model.AuthTypeZAIOAuth {
				want[0].URL = site.next
				want[1].URL = site.next + "/v1/messages"
				want[0].Protocols = []string{"anthropic", "openai"}
				want[1].Protocols = nil
				want = want[:4]
			}
			samples = append(samples, sample{id: cfg.ID, urls: want, disabled: []string{want[0].RuntimeURL(), want[1].RuntimeURL()}, auth: auth})
		}
	}
	if _, err := store.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", zaiDirectURLsMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err = createSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		states, err := store.LoadDisabledURLs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, sample := range samples {
			cfg, err := store.GetConfig(ctx, sample.id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.URLs, sample.urls) || cfg.AuthType != sample.auth || cfg.Enabled || (sample.auth == model.AuthTypeZAIOAuth && cfg.OAuthCredential != `{"api_key":"unchanged"}`) {
				t.Fatalf("migration changed unrelated config: id=%d urls=%+v", sample.id, cfg.URLs)
			}
			got := states[sample.id]
			if len(got) != len(sample.disabled) {
				t.Fatalf("disabled URLs=%v want=%v", got, sample.disabled)
			}
			for _, want := range sample.disabled {
				found := false
				for _, actual := range got {
					if actual == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing disabled URL %s in %v", want, got)
				}
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
