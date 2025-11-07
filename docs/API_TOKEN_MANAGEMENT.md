# API访问令牌管理功能使用指南

## 功能概述

本功能允许通过Web管理界面动态管理用于访问代理API的认证令牌，无需重启服务即可立即生效。

## 核心特性

✅ **即时生效**：创建/删除/禁用令牌后立即生效（热更新）  
✅ **安全存储**：数据库存储SHA256哈希，而非明文  
✅ **过期管理**：支持设置过期时间或永不过期  
✅ **脱敏显示**：日志和界面仅显示前4后4字符  
✅ **使用追踪**：记录最后使用时间

## 快速开始

### 1. 访问管理界面

启动服务后，访问：
```
http://localhost:8080/web/tokens.html
```

或从首页点击"令牌管理"卡片进入。

### 2. 创建第一个令牌

1. 点击"+ 创建令牌"按钮
2. 填写描述（例如："生产环境API令牌"）
3. 选择过期时间（可选）：
   - 永不过期
   - 30/90/180/365天后过期
   - 自定义日期
4. 确认启用状态（默认启用）
5. 点击"创建"

**⚠️ 重要：** 明文令牌仅在创建时显示一次，请立即复制保存！

### 3. 使用令牌访问API

创建的令牌可以立即用于访问代理API：

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer YOUR_TOKEN_HERE" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

## API端点文档

### 列出所有令牌
```
GET /admin/auth-tokens
Authorization: Bearer {admin_token}
```

**响应示例：**
```json
{
  "success": true,
  "data": [
    {
      "id": 1,
      "token_masked": "sk-a****mnop",
      "description": "生产环境令牌",
      "created_at": "2025-11-07T19:00:00Z",
      "expires_at": null,
      "last_used_at": 1699380000000,
      "is_active": true,
      "is_expired": false,
      "is_valid": true
    }
  ]
}
```

### 创建新令牌
```
POST /admin/auth-tokens
Authorization: Bearer {admin_token}
Content-Type: application/json
```

**请求体：**
```json
{
  "description": "测试令牌",
  "expires_at": 1704067200000,  // 可选，Unix毫秒时间戳
  "is_active": true              // 可选，默认true
}
```

**响应示例：**
```json
{
  "success": true,
  "data": {
    "id": 2,
    "token": "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6",  // 仅此一次返回！
    "description": "测试令牌",
    "created_at": "2025-11-07T19:00:00Z",
    "expires_at": 1704067200000,
    "is_active": true
  }
}
```

### 更新令牌
```
PUT /admin/auth-tokens/:id
Authorization: Bearer {admin_token}
Content-Type: application/json
```

**请求体：**
```json
{
  "description": "更新后的描述",
  "is_active": false,           // 禁用令牌
  "expires_at": 1709251200000   // 更新过期时间
}
```

### 删除令牌
```
DELETE /admin/auth-tokens/:id
Authorization: Bearer {admin_token}
```

**响应示例：**
```json
{
  "success": true,
  "data": {
    "id": 2
  }
}
```

## 常见场景

### 场景1：紧急禁用泄露的令牌

如果发现令牌泄露：

1. 登录Web管理界面
2. 找到对应令牌
3. 点击"编辑"
4. 取消勾选"启用令牌"
5. 保存

**效果：** 立即生效，该令牌无法再访问API。

### 场景2：临时令牌

为临时测试创建30天有效期的令牌：

1. 创建令牌时选择"30天后过期"
2. 保存明文令牌用于测试
3. 30天后自动失效，无需手动删除

### 场景3：批量替换令牌

安全策略要求定期轮换：

1. 创建新令牌记录明文
2. 更新客户端配置使用新令牌
3. 验证新令牌正常工作
4. 删除旧令牌

## 安全最佳实践

### ✅ 推荐做法

1. **描述清晰**：令牌描述包含用途和负责人（例如："生产API - 张三"）
2. **定期轮换**：建议每90天轮换一次令牌
3. **最小权限**：未来版本将支持令牌权限范围限制
4. **监控使用**：定期检查`last_used_at`，删除未使用的令牌
5. **设置过期**：非永久使用的令牌应设置合理过期时间

### ❌ 避免做法

1. **明文存储**：不要将明文令牌写入配置文件或代码
2. **过度共享**：避免多个应用共用同一个令牌
3. **无限期令牌**：除非必要，避免创建永不过期的令牌
4. **忽略审计**：定期检查令牌使用情况

## 技术细节

### Token生成算法
```
1. 生成32字节随机数据（crypto/rand）
2. Base64编码为64字符十六进制字符串
3. 计算SHA256哈希存储到数据库
4. 明文Token仅在响应中返回一次
```

### 热更新机制
```
每次CRUD操作后 → AuthService.ReloadAuthTokens()
                → 重新加载数据库活跃令牌
                → 更新内存缓存
                → <100ms生效
```

### 数据库结构
```sql
CREATE TABLE auth_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT NOT NULL UNIQUE,           -- SHA256哈希
    description TEXT NOT NULL,
    created_at INTEGER NOT NULL,          -- Unix毫秒
    expires_at INTEGER,                   -- Unix毫秒，NULL=永不过期
    last_used_at INTEGER,                 -- Unix毫秒
    is_active INTEGER NOT NULL DEFAULT 1, -- 0=禁用，1=启用
    CHECK (is_active IN (0, 1))
);
```

## 故障排除

### 问题1：令牌创建后无法使用

**排查步骤：**
1. 检查Token是否被正确复制（无多余空格）
2. 确认令牌状态为"正常"（未过期且已启用）
3. 查看服务日志确认热更新成功：
   ```
   🔄 API令牌已热更新（N个有效令牌）
   ```

### 问题2：无法访问管理界面

**可能原因：**
- 未登录管理后台（需要`CCLOAD_PASS`认证）
- 缺少管理员Token

**解决方法：**
```bash
# 登录管理后台
http://localhost:8080/login

# 使用CCLOAD_PASS密码登录
```

### 问题3：热更新失败

**日志提示：**
```
⚠️  热更新失败: ...
```

**影响：** 令牌已保存到数据库，但需要重启服务才能生效。

**解决方法：**
1. 检查数据库连接是否正常
2. 重启服务：`make dev` 或 `./ccload`

## 迁移指南

### 从环境变量迁移

如果当前使用`CCLOAD_AUTH=token1,token2`：

1. **保留环境变量**（向后兼容）
2. **迁移到数据库**：
   ```bash
   # 为每个现有Token创建数据库记录
   curl -X POST http://localhost:8080/admin/auth-tokens \
     -H "Content-Type: application/json" \
     -d '{"description": "迁移：token1"}'
   ```
3. **验证新Token**：确认可以正常访问API
4. **清理环境变量**：从`.env`中移除`CCLOAD_AUTH`

## 更新日志

### v2.5.0 (2025-11-07)
- ✅ 新增Web管理界面
- ✅ 实现热更新机制
- ✅ 支持过期时间管理
- ✅ SHA256哈希存储
- ✅ 脱敏显示

---

**相关文档：**
- [README.md](../README.md) - 项目整体文档
- [CLAUDE.md](../CLAUDE.md) - 开发指南
