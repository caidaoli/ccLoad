# 异步验证器架构设计

## 当前问题

验证器在请求热路径上**同步等待**外部 API：
```go
// 每个代理请求都阻塞等待外部 API 返回（最多 3 秒）
available, _, _ := validator.Validate(ctx, cfg, apiKey)
```

**影响**：
- 88code API 抖动 → 你的服务 P99 抖动
- 88code API 故障 → 降级 fail-open，但每次请求都要等 3 秒超时

---

## 方案设计：异步验证 + 结果缓存

### 核心思路

```
同步路径（请求热路径）:
  请求 → 读取验证缓存 → 立即返回（零延迟）

异步路径（后台 worker）:
  定期刷新验证结果 → 更新缓存（例如每 60 秒）
```

### 架构改动

**1. SubscriptionValidator 改造**

```go
type SubscriptionValidator struct {
    // 现有字段...

    // 异步验证
    refreshInterval time.Duration      // 刷新间隔（例如 60s）
    workerDone      chan struct{}      // 优雅关闭信号
    wg              sync.WaitGroup     // 等待 worker 退出

    // 验证结果缓存（key 级）
    validationCache sync.Map // "channelID:apiKey" → validationResult
}

type validationResult struct {
    available bool
    reason    string
    timestamp time.Time
}
```

**2. 新增方法**

```go
// StartAsyncValidation 启动异步验证 worker
func (v *SubscriptionValidator) StartAsyncValidation(channels []*model.Config) {
    v.wg.Add(1)
    go v.validationWorker(channels)
}

// validationWorker 后台刷新验证结果
func (v *SubscriptionValidator) validationWorker(channels []*model.Config) {
    defer v.wg.Done()

    ticker := time.NewTicker(v.refreshInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            v.refreshValidationResults(channels)
        case <-v.workerDone:
            return
        }
    }
}

// Validate 改为读取缓存（零延迟）
func (v *SubscriptionValidator) Validate(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
    cacheKey := fmt.Sprintf("%d:%s", cfg.ID, apiKey)

    // 读取缓存
    if result, ok := v.validationCache.Load(cacheKey); ok {
        vr := result.(validationResult)
        return vr.available, vr.reason, nil
    }

    // 缓存未命中：乐观策略，允许通过（后台会刷新）
    return true, "", nil
}
```

**3. 集成到系统**

```go
// Server 启动时
validatorManager := validator.NewManager()
subscriptionValidator := validator.NewSubscriptionValidator(enabled)

// 获取所有 88code 渠道
channels := getAll88CodeChannels()

// 启动异步验证
subscriptionValidator.StartAsyncValidation(channels)
validatorManager.AddValidator(subscriptionValidator)
```

---

## 收益

| 指标 | Before（同步） | After（异步） | 改进 |
|------|---------------|--------------|------|
| 请求延迟 | +0~3000ms | +0ms | **3000 倍** |
| P99 延迟 | 受外部 API 影响 | 稳定 | **解耦** |
| 验证频率 | 每次请求 | 每 60 秒 | API 调用减少 **99%** |
| 故障降级 | 3 秒超时 | 读取上次结果 | **优雅降级** |

---

## 权衡

**优点**：
- ✅ 零请求延迟（读缓存，微秒级）
- ✅ 外部 API 故障不影响服务性能
- ✅ API 调用次数大幅减少（from 每请求 to 每分钟）

**缺点**：
- ❌ 验证结果有延迟（最多 60 秒旧数据）
- ❌ 复杂度增加（需要管理 worker 生命周期）
- ❌ 初次启动时缓存为空（可用乐观策略或同步初次验证）

---

## 建议

**当前修复已足够**：
- ✅ 超时收紧到 3 秒（10 倍改进）
- ✅ 告警日志恢复（可观测性）
- ✅ 缓存 60 秒（减少 API 调用）

**异步验证是架构优化，不是 bug 修复**：
- 适用场景：88code 渠道数量大（>10 个）且请求 QPS 高（>100）
- 当前场景：如果只有少量 88code 渠道，同步验证 + 缓存已经够用
- 建议：先观察生产环境，如果 88code 相关请求的 P99 仍然不理想，再考虑异步化

---

## 是否实现？

这是一个**架构级改动**，需要：
- 新增 150+ 行代码
- 修改启动流程
- 管理 worker 生命周期

**建议**：
1. **保守方案**：当前修复已足够（3 秒超时 + 缓存 + 日志）
2. **激进方案**：实现异步验证器（需要 1-2 小时，测试覆盖）

你的选择？
