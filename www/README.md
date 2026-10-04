# ccLoad 介绍网站

ccLoad 项目的官方介绍网站。执行 `make www-setup` 复制共享资源后，它就是**完全独立的静态网站**，可以部署到任何 Web 服务器。

## 功能特性

- ✅ **构建期预渲染**：`build.mjs` 把每页渲染成英文 `/` 与中文 `/zh/` 两份完整 HTML，爬虫无需执行 JS 即可读到正文
- ✅ **SEO 完整**：title/description、canonical、hreflang 互指、Open Graph/Twitter、JSON-LD（首页含 FAQPage）、`sitemap.xml`、`robots.txt`
- ✅ **语言偏好**：英文页对 zh 浏览器且无保存偏好的访客跳转到 `/zh/`；手动切换写入 `localStorage.ccload_locale`
- ✅ **主题切换**：light/dark/system 三种模式
- ✅ **零依赖**：构建脚本只用 Node 内置模块，运行时只有导航、复制、Tab 三类交互脚本

## 快速开始

```bash
make www-run        # www-setup + www-build，然后在 www/dist 上启动预览，访问 http://localhost:8888/
make www-build      # 仅构建到 www/dist/
make www-release    # 构建并 rsync www/dist/ 到线上
```

`www/dist/` 是唯一的发布产物，源 HTML、语言包和 `promo/` 不会上线。详细部署见 [DEPLOY.md](DEPLOY.md)。

## 开发指南

### 文案来源

- **英文**：直接写在源 HTML 里（`index.html` 等），即英文页的最终文本
- **中文**：首页、导航、页脚、页面 title 写在 `assets/locales/zh-CN.js`；子页正文写在 `assets/locales/<page>.zh-CN.js`
- **导航/页脚英文**：由构建脚本生成，文案在 `assets/locales/en.js`

### 标注方式

```html
<h2 data-i18n="www.home.why.title">Why ccLoad</h2>              <!-- 纯文本，构建时转义 -->
<p data-i18n-html="www.install.docker.desc">Set <code>X</code></p> <!-- 允许行内 HTML -->
<img data-i18n-alt="www.home.preview.alt" alt="...">              <!-- 属性：alt/aria-label/title/content/src/poster -->
```

中文构建是严格模式：任何 `data-i18n*` 键在中文语言包里缺失，`make www-build` 直接失败。改了英文源文要同步改对应中文键。所有 `data-i18n*` 属性在产物中被剥离。

### 添加新页面

1. 在 `www/` 创建 HTML，`<title>` 用 `data-i18n="www.<page>.meta.title"`，并写 `<meta name="description">`
2. 在 `build.mjs` 的 `PAGES` 与 `NAV_ITEMS` 中加入该页，`en.js` 与 `zh-CN.js` 加导航文案
3. 添加中文键，运行 `make verify-web`（`build.test.mjs` 校验死链、hreflang、sitemap、JSON-LD）

### 样式

在 `assets/css/www.css` 中添加，使用 `www-` 前缀；共享设计系统 `styles.css` 由 `make www-setup` 从 `web/` 复制。

## 技术栈

- **构建**：`build.mjs`（Node ESM，零依赖）预渲染双语静态页
- **前端**：原生 JavaScript，`nav.js` / `www.js` 只负责交互
- **样式**：CSS3 + CSS 变量
- **后端**：无；任意静态 Web 服务器托管 `dist/`

## 已完成功能

### ✅ 首页（index.html）
- Hero 区域：定位 H1、一句话价值、数据亮点
- ccLoad 是什么（介绍视频）、为什么选 ccLoad（6 项差异点）、适用人群
- FAQ（同步输出 FAQPage 结构化数据）
- 核心特性卡片（OAuth、思考后缀、Key 模型白名单、渠道时段）
- 第一方账号渠道：Codex / Anthropic / Antigravity / xAI / CodeBuddy / Z.ai / Cursor / Zed
- 管理后台预览截图
- 5 种部署方式卡片（Go 1.26+，官方 latest 二进制）
- 快速开始 Tab 切换
- 代码复制功能

### ✅ 安装指南（install.html）
- Docker Compose 部署（GHCR latest / beta / 精确版本）
- Homebrew 安装、密码配置、后台服务及稳定版升级
- Hugging Face Spaces 部署
- 源码编译与二进制运行（含 Cursor SDK Bridge）
- 部署后验证命令

### ✅ 配置手册（config.html）
- 启动环境变量表（含 PORT、CURSOR_SDK_BRIDGE_BIN、TRUSTED_PROXIES）
- SQLite / MySQL / PostgreSQL / Hybrid 存储模式对比
- 渠道配置、OAuth auth_type、Key 模型白名单、单模型启停、可用时段、每日定时检测
- 思考后缀、全局 OAuth 上游地址、冷却兜底
- 全局流式总超时、首字节/非流式协议覆盖、上游连接复用时限和 WebSocket 会话限制（1024 会话 / 256 MiB）
- 批量模型名小写与来源前缀清理
- API Token 模型、渠道白名单/黑名单、费用和并发限制
- 自定义模型价格：基础/高上下文输入、输出和缓存价，覆盖恢复及适用边界；渠道模型价格按渠道单独覆盖
- 管理后台热配置说明
- 安全上线检查

### ✅ API 使用（usage.html）
- Anthropic / OpenAI / Gemini / Codex / Images / Token 统计示例
- 思考后缀、第一方账号渠道、Images API
- HTTP 与 Responses WebSocket 端点速查表、会话隔离、连接轮换和故障切换边界
- 多协议内置搜索映射说明
- Antigravity Responses 原生搜索、域名过滤与引用返回
- 管理后台的客户端协议统计、7 天服务健康度、缓存统计与模型测试 Key 备注/倍率
- 渠道管理 API 与 CSV 导入导出

### ✅ 反馈渠道（feedback.html）
- Bug、功能建议、讨论、安全问题分流
- 贡献代码入口
- 高质量反馈模板

### ✅ 基础组件
- 导航栏（支持移动端汉堡菜单）
- 语言切换器
- 主题切换器
- 代码块（带复制按钮）
- Tab 切换
- 页脚

## 文档

项目主文档请查看：[GitHub README](https://github.com/caidaoli/ccLoad/blob/master/README.md)。

## 贡献

欢迎贡献代码和内容！

1. Fork 项目
2. 创建特性分支 (`git checkout -b feature/amazing-feature`)
3. 提交更改 (`git commit -m 'Add some amazing feature'`)
4. 推送到分支 (`git push origin feature/amazing-feature`)
5. 创建 Pull Request

## License

MIT License - 与 ccLoad 项目保持一致
