# GPT-5.6 长上下文分段计价修复设计

## 根因

计费链路把两个不同概念合并成了一个 `inputTokens`：

- 协议解析层从 OpenAI 总输入 Token 中扣除缓存命中 Token，得到实际按普通输入价计费的非缓存输入。
- 成本计算层又使用这个已扣减值判断上下文是否超过 272K。

OpenAI 的长上下文分档取决于整次请求的总输入长度，缓存命中只改变对应 Token 的单价，不改变它们属于输入上下文这一事实。因此，总输入超过 272K、但大部分输入命中缓存时，当前实现会错误选择短上下文价格。

另一个可靠性缺口是内置 GPT-5.6 价格只有基础价。部署若没有有效模型目录缓存，且远端同步失败或被禁用，GPT-5.6 长上下文请求会始终按低档价格计算。

## 目标

- GPT-5.6、GPT-5.6 Sol、Terra、Luna 使用 272K 输入 Token 作为整次请求的价格分界。
- OpenAI context tier 使用 `非缓存输入 + 缓存读取输入` 判断档位。
- 选中长上下文档后，整次请求的普通输入、缓存读取、缓存写入和输出均使用长上下文价格。
- 即使没有远端模型目录或本地缓存，内置价格也能正确计算 GPT-5.6 两档费用。
- 保持 `CalculateCostDetailed` 的公开签名和现有调用方不变。

## 非目标

- 不修改 usage 持久化字段或 API 返回结构。
- 不重构协议解析器的可计费 Token 归一化逻辑。
- 不引入独立的总输入 Token 参数。
- 不修改 GPT-5.4、GPT-5.5、Gemini、Qwen、MiMo 等模型现有分档语义。
- 不改变 OpenAI service tier 倍率逻辑。

## 设计

### 分档语义

复用现有 `ModelPricing.CacheReadCountsTowardTier` 字段。该字段表示缓存读取 Token 是否参与价格档位判断，而不是表示缓存读取 Token 按普通输入价收费。

成本仍按现有结构分别计算：

- 非缓存输入使用所选档位的 `InputPrice`。
- 缓存读取使用所选档位的 `CacheReadPrice`。
- 缓存写入使用所选档位输入价格的 1.25 倍。
- 输出使用所选档位的 `OutputPrice`。

只有档位选择使用 `inputTokens + cacheReadTokens`。这样既保留解析层避免重复计费的职责，也能用正确的总上下文长度选择价格。

### 内置 GPT-5.6 价格

为 GPT-5.6 系列定义三个可复用的 `TokenPricingTier` 切片：

- Sol 和裸 `gpt-5.6`：短档 `5 / 30 / 0.5`，长档 `10 / 45 / 1`。
- Terra：短档 `2.5 / 15 / 0.25`，长档 `5 / 22.5 / 0.5`。
- Luna：短档 `1 / 6 / 0.1`，长档 `2 / 9 / 0.2`。

每组第一档的闭区间上限为 `272_000`，第二档无上限。四个内置模型都设置 `CacheReadCountsTowardTier: true`。

缓存写入价格不新增字段。当前 GPT-5.6 官方价格始终是所选档位输入价的 1.25 倍，继续使用现有公式，避免重复存储派生数据。

### 远端模型目录

`models.dev` 的 OpenAI context tier 在归一化时设置 `CacheReadCountsTowardTier: true`。非 OpenAI provider 保持现状，避免改变 Gemini 等仅按非缓存输入判断档位的既有语义。

远端精确模型价格覆盖内置 tiers 时，保留内置模型的分档计数语义；远端新增的 OpenAI context-tier 模型也能获得同样的正确行为。

## 错误处理

- 远端目录不可用：使用完整的内置 GPT-5.6 分段价格。
- 远端目录存在但没有 context tiers：沿用远端基础价，不凭模型名推测不存在的档位。
- Token 数为负数或模型未知：保持现有返回行为。

## 测试

复用现有测试文件，不新增测试文件：

- 扩展 `internal/util/cost_calculator_test.go`，验证 GPT-5.6、Sol、Terra、Luna 在 272K 边界两侧的价格。
- 增加缓存回归场景：非缓存输入低于 272K，但加上缓存读取后超过 272K，必须选择长上下文档。
- 验证长档缓存写入仍按长档输入价的 1.25 倍计算。
- 扩展 `internal/util/models_dev_catalog_test.go`，验证 OpenAI context tier 自动启用缓存参与分档，其他 provider 不受影响。
- 保留并运行现有模型目录覆盖、模糊匹配和 service tier 测试。

验证命令：

```bash
go test -tags sonic ./internal/util/...
go test -tags sonic ./internal/...
golangci-lint run ./...
```
