# CodeBuddy 协议来源

参考 [lovingfish/workbuddy-cliproxy](https://github.com/lovingfish/workbuddy-cliproxy)，固定提交 `7efb280563b8cf2bf62e4295340708bac4ec3d6a`。上游 MIT 许可见 [LICENSE](LICENSE)。

本包按其 CodeBuddy CLI 协议实现登录、轮询、账号信息与 Token 刷新。渠道管理、凭证 CAS 持久化、超时、SSE 读取、协议转换与用量解析由 ccLoad 实现；未引入上游 HTTP 服务或独立转换核心。

新建渠道的初始模型列表来自该提交，不代表账号实际可用模型。

“获取模型”使用官方 `@tencent-ai/codebuddy-code@2.148.0` 的 `CloudProductProvider` / `CloudProductManagerImpl` 契约：携带账号凭证 GET `https://copilot.tencent.com/v3/config`，读取 `data.models[].id`。官方安装包版本通过 npm registry 核实，协议从本机同版本官方发布包读取。该入口按实时响应返回模型，不使用初始列表过滤或错误回退；没有接入额度接口。

`data.models` 是模型定义总目录，不等于 CLI 可调用清单。模型范围以 `data.agents` 中 `name="cli"` 的 `models` 为准，与模型定义按完整 ID 匹配，移除 `disabled:true`，并对非空 `availableModels` 取交集、去重。不维护固定模型排除名单，也不根据别名、标签或 `disabledMultimodal` 扩展或排除模型。CLI 列表缺失、为空或筛选后无模型时返回错误，不回退到完整目录或内置列表。上游 `11102 / service info not found` 表示模型无服务配置；目录筛选不能保证账号余额、权限或服务实时健康。
