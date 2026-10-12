# ccLoad 介绍网站

ccLoad 项目的官方介绍网站。执行 `make www-setup` 复制共享资源后，它就是**完全独立的静态网站**，可以部署到任何 Web 服务器。

## 功能特性

- ✅ **构建期预渲染**：`build.mjs` 把每页渲染成英文 `/` 与中文 `/zh/` 两份完整 HTML，爬虫无需执行 JS 即可读到正文
- ✅ **SEO 完整**：title/description、canonical、hreflang 互指、Open Graph/Twitter、JSON-LD（首页含 FAQPage）、`sitemap.xml`、`robots.txt`
- ✅ **语言偏好**：英文页对 zh 浏览器且无保存偏好的访客跳转到 `/zh/`；手动切换写入 `localStorage.ccload_locale`
- ✅ **主题切换**：light/dark/system 三种模式
- ✅ **自动缓存失效**：构建时按文件内容给本地资源 URL 附 `?v=<hash>`，内容不变 URL 不变，源 HTML 不手写版本号
- ✅ **零依赖**：构建脚本只用 Node 内置模块，运行时脚本只处理导航、复制、Tab、渐入与文档目录高亮

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
<img data-i18n-alt="www.home.preview.alt" alt="...">              <!-- 属性：alt/aria-label/title/content/src/poster/srclang/label -->
```

中文构建是严格模式：任何 `data-i18n*` 键在中文语言包里缺失，`make www-build` 直接失败。改了英文源文要同步改对应中文键。所有 `data-i18n*` 属性在产物中被剥离。

### 添加新页面

1. 在 `www/` 创建 HTML，`<title>` 用 `data-i18n="www.<page>.meta.title"`，并写 `<meta name="description">`
2. 在 `build.mjs` 的 `PAGES` 与 `NAV_ITEMS` 中加入该页，`en.js` 与 `zh-CN.js` 加导航文案
3. 添加中文键，运行 `make verify-web`（`build.test.mjs` 校验死链、hreflang、sitemap、JSON-LD）

### 样式

在 `assets/css/www.css` 中添加，使用 `www-` 前缀。`www.css` 开头自带设计令牌（颜色、字体、间距、圆角），不依赖管理后台的 `styles.css`；`make www-setup` 只从 `web/` 复制 `theme-init.js` 和图标。

图标不用 emoji：源 HTML 写 `<span class="www-feature-icon" data-icon="key"></span>`（也可用 `www-doc-icon`），构建时由 `build.mjs` 的 `ICONS` 内联为线性 SVG；图标名未知时构建直接失败。

子页面布局由构建生成：`<header class="www-page-hero">` 自动加面包屑；带 `id` 的 `<section>`（取其 `h2`）不少于 3 个时，生成左侧本页导航，`<section data-toc-icon="名称">` 为条目配 `ICONS` 图标。多行代码块自动加行号栏。

## 技术栈

- **构建**：`build.mjs`（Node ESM，零依赖）预渲染双语静态页
- **前端**：原生 JavaScript，`nav.js` / `www.js` 只负责交互
- **样式**：CSS3 + CSS 变量
- **后端**：无；任意静态 Web 服务器托管 `dist/`

## 页面

| 页面 | 内容 |
| --- | --- |
| `index.html` | 定位与介绍视频、差异点、同类对比、适用人群、能力、账号渠道、后台预览、快速开始、FAQ |
| `install.html` | Docker / Hugging Face / Homebrew / 源码与二进制部署，部署后验证 |
| `config.html` | 环境变量、存储模式、渠道与令牌配置、运行时设置、上线检查 |
| `usage.html` | 各协议调用示例、端点速查、Responses WebSocket、管理 API |
| `feedback.html` | 问题分流、贡献入口、反馈模板 |

## 图片与视频

截图是入库的 WebP 变体，页面用 `srcset` 按宽度选择。仓库根目录 `images/` 的原图更新后，在仓库根目录重新生成：

```bash
for n in dashboard logs; do for w in 1280 2048; do
  cwebp -quiet -q 85 -m 6 -resize $w 0 images/ccload-$n.webp -o www/assets/images/ccload-$n-$w.webp
done; done
```

宽高比变了要同步 HTML 中 `<img>` 的 `width`/`height`。介绍视频 `.mp4` 不入库，生成方法见 [DEPLOY.md](DEPLOY.md#-介绍视频)；`promo/audio.sh` 同时按旁白时间轴写出同名 `.vtt` 字幕，字幕入库。

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
