package sqlite

import (
    "context"
    "sync"
    "testing"
    "time"

    "ccLoad/internal/model"
)

// TestGetAndSetChannelRRIndex_CAS_Concurrent 验证并发下指针推进不会退化
func TestGetAndSetChannelRRIndex_CAS_Concurrent(t *testing.T) {
    tmp := t.TempDir()
    store, err := NewSQLiteStoreForTest(tmp+"/test.db", nil)
    if err != nil {
        t.Fatalf("new store: %v", err)
    }
    ctx := context.Background()

    // 创建一个渠道
    cfg, err := store.CreateConfig(ctx, &model.Config{
        Name:        "rr-cas",
        URL:         "https://upstream",
        Priority:    1,
        Models:      []string{"m"},
        ChannelType: "anthropic",
        Enabled:     true,
    })
    if err != nil {
        t.Fatalf("create config: %v", err)
    }

    keyCount := 5
    now := time.Now()
    for i := 0; i < keyCount; i++ {
        _ = store.CreateAPIKey(ctx, &model.APIKey{
            ChannelID: cfg.ID,
            KeyIndex:  i,
            APIKey:    "sk-" + time.Now().Format("150405") + string(rune('a'+i)),
            CreatedAt: model.JSONTime{Time: now},
            UpdatedAt: model.JSONTime{Time: now},
        })
    }

    // 高并发推进指针
    const N = 200
    var wg sync.WaitGroup
    wg.Add(N)
    idxCh := make(chan int, N)

    for i := 0; i < N; i++ {
        go func() {
            defer wg.Done()
            idx, err := store.GetAndSetChannelRRIndex(ctx, cfg.ID, keyCount)
            if err == nil {
                idxCh <- idx
            }
        }()
    }
    wg.Wait()
    close(idxCh)

    // 统计分布
    counts := make(map[int]int)
    total := 0
    for v := range idxCh {
        counts[v]++
        total++
    }

    if total == 0 {
        t.Fatalf("no indices returned")
    }

    // 要求：至少出现过一半以上的不同索引；且最大桶不超过总数的80%
    if len(counts) < keyCount/2 {
        t.Fatalf("too few unique indices: got=%d want>=%d", len(counts), keyCount/2)
    }
    max := 0
    for _, c := range counts {
        if c > max {
            max = c
        }
    }
    if float64(max) > float64(total)*0.8 {
        t.Fatalf("distribution too skewed: max=%d total=%d", max, total)
    }
}

