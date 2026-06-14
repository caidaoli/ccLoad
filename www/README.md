# ccLoad 介绍网站

ccLoad 项目的官方介绍网站，**完全独立的静态网站**，可以复制到任何 Web 服务器使用。

## 功能特性

- ✅ **完全独立**：可直接复制到 Nginx/Apache/CDN
- ✅ **中英双语**：自动检测浏览器语言，支持手动切换
- ✅ **主题切换**：支持 light/dark/system 三种模式
- ✅ **响应式设计**：完美适配移动端、平板、桌面
- ✅ **零框架依赖**：纯原生 JavaScript，轻量高效（< 20KB）

## 快速开始

### 开发环境（ccLoad 项目内）

```bash
# 1. 设置网站（复制共享资源）
make www-setup

# 2. 本地预览
make www-run

# 访问 http://localhost:8888/
```

### 部署到生产环境

```bash
# 1. 设置网站
make www-setup

# 2. 复制到你的 Web 服务器
cp -r www /path/to/webroot/

# 完成！现在可以通过 Web 服务器访问
```

详细部署指南请查看 [DEPLOY.md](DEPLOY.md)。

## 开发指南

### 添加新页面

1. 在 `www/` 目录创建新的 HTML 文件
2. 在 `www/assets/js/nav.js` 的 `NAV_ITEMS` 中添加导航项
3. 在语言包中添加对应的翻译

### 添加翻译

在 `www/assets/locales/zh-CN.js` 和 `en.js` 中添加翻译条目：

```javascript
// 中文
'www.page.title': '页面标题',

// 英文
'www.page.title': 'Page Title',
```

### 使用 i18n

在 HTML 中使用 `data-i18n` 属性：

```html
<h1 data-i18n="www.page.title">页面标题</h1>
```

### 样式开发

在 `www/assets/css/www.css` 中添加样式，使用 `www-` 前缀避免冲突：

```css
.www-my-component {
  /* 样式规则 */
}
```

## 技术栈

- **前端**：纯原生 JavaScript ES6+
- **样式**：CSS3 + CSS 变量
- **国际化**：自研 i18n 系统
- **构建**：Go embed.FS 嵌入
- **后端**：Gin + Go 1.25+

## 已完成功能

### ✅ 首页（index.html）
- Hero 区域
- 8 个核心特性卡片
- 4 种部署方式卡片
- 快速开始 Tab 切换
- 代码复制功能

### ✅ 反馈渠道（feedback.html）
- GitHub Issues 链接
- GitHub Discussions 链接
- Star 支持链接

### ✅ 基础组件
- 导航栏（支持移动端汉堡菜单）
- 语言切换器
- 主题切换器
- 代码块（带复制按钮）
- Tab 切换
- 页脚

## 待完善功能

### 🚧 安装指南（install.html）
需要补充：
- Docker 部署详细步骤
- Hugging Face 部署说明
- 源码编译流程
- 二进制下载和使用

### 🚧 配置文档（config.html）
需要补充：
- 环境变量配置表
- 数据库选择对比
- 渠道配置说明

### 🚧 使用指南（usage.html）
需要补充：
- API 调用示例（Claude/Codex/Gemini/OpenAI）
- 管理界面使用说明

## 文档

详细的实施报告请查看：[docs/www-implementation-report.md](../docs/www-implementation-report.md)

## 贡献

欢迎贡献代码和内容！

1. Fork 项目
2. 创建特性分支 (`git checkout -b feature/amazing-feature`)
3. 提交更改 (`git commit -m 'Add some amazing feature'`)
4. 推送到分支 (`git push origin feature/amazing-feature`)
5. 创建 Pull Request

## License

MIT License - 与 ccLoad 项目保持一致
