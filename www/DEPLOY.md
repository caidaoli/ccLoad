# ccLoad 介绍网站部署指南

这是 ccLoad 项目的独立介绍网站。源文件在 `www/`，发布产物是 `make www-build` 生成的 `www/dist/`（纯静态，可部署到任何 Web 服务器）。

## 📁 构建产物

```bash
make www-build   # = make www-setup（复制共享资源）+ node www/build.mjs
```

`www/dist/` 包含：

- `index.html` 等英文页（`x-default`），`zh/` 下为对应中文页
- `sitemap.xml`（含 hreflang 交替链接）、`robots.txt`
- `<INDEXNOW_KEY>.txt`：IndexNow 站点归属证明；`make www-release` 同步后自动执行 `node www/build.mjs --indexnow`，把全部页面推给 Bing、Yandex 等
- `assets/{css,js,images,video}`、favicon 与品牌图标

不包含：源 HTML、`assets/locales/`、`build.mjs`、`promo/`。站点域名由 `build.mjs` 的 `SITE_URL` 决定（canonical、sitemap、OG 都依赖它），换域名时改这里。

## 🚀 部署

### 官方站点

```bash
make www-release   # 构建后 rsync --delete www/dist/ 到线上目录
```

### Nginx

```nginx
server {
    listen 80;
    server_name your-domain.com;
    root /var/www/ccload/dist;   # 指向 www/dist 的拷贝
    index index.html;

    location / {
        try_files $uri $uri/ =404;
    }

    location ~* \.(css|js|jpg|jpeg|png|gif|svg|ico|mp4|woff2?)$ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }

    location ~* (\.html|/)$ {
        add_header Cache-Control "no-cache, must-revalidate";
    }
}
```

CSS/JS 通过 `?v=` 查询串做缓存失效，改动样式或脚本时同步更新 HTML 中的版本号。

### 其他静态托管（GitHub Pages / Netlify / Vercel / Cloudflare Pages）

把 `www/dist/` 作为发布目录即可，例如 `netlify deploy --prod --dir www/dist`。无需任何重写规则。

## 📝 源码结构

```
www/
├── build.mjs               # 预渲染构建脚本（零依赖 Node ESM）
├── build.test.mjs          # 构建产物测试（make verify-web 运行）
├── index.html / install.html / config.html / usage.html / feedback.html  # 英文源文 + data-i18n 标注
├── assets/
│   ├── css/www.css         # 网站专用样式（styles.css 由 www-setup 复制）
│   ├── js/nav.js           # 移动菜单、主题、语言偏好
│   ├── js/www.js           # 代码复制、Tab、平滑滚动
│   ├── images/             # Hero 背景、截图、视频封面
│   ├── video/              # 介绍视频（不入库）
│   └── locales/
│       ├── en.js           # 构建生成的导航/页脚英文
│       ├── zh-CN.js        # 首页、导航、页脚、页面标题中文
│       └── <page>.zh-CN.js # 子页正文中文
├── promo/                  # 介绍视频源
└── dist/                   # 构建产物（gitignore）
```

## 🎬 介绍视频

`assets/video/*.mp4` 不入库。新克隆的仓库在部署前需要先生成（依赖 ego-browser、ffmpeg、uvx）：

```bash
(cd www && python3 -m http.server 8765 &)
ego-browser nodejs -e "globalThis.PROMO={lang:'zh',out:'/tmp/promo-zh'};$(cat www/promo/render.mjs)"
ffmpeg -framerate 30 -i /tmp/promo-zh/f%05d.jpg -c:v libx264 -crf 18 -preset slow \
  -pix_fmt yuv420p -movflags +faststart www/assets/video/ccload-promo.zh-CN.mp4
www/promo/audio.sh zh    # 配旁白和背景音乐，原地替换
```

英文版把 `zh` 换成 `en`，输出 `ccload-promo.en.mp4`。`make www-release` 在视频缺失时会直接失败，因为 rsync 带 `--delete`，缺文件发布会删掉线上视频。

## 🔍 验证部署

- `https://your-domain.com/` 与 `/zh/` 均返回完整正文（`curl` 可见，无需 JS）
- `https://your-domain.com/sitemap.xml`、`/robots.txt` 可访问
- 上线后在 Google Search Console 与百度搜索资源平台提交 sitemap

## 🐛 故障排除

- **`make www-build` 报 missing translations**：中文语言包缺少报错中列出的键
- **报 missing favicon.svg 等**：先执行 `make www-setup`
- **样式未加载**：确认部署的是 `dist/` 而不是 `www/` 源目录

## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

## 📄 License

MIT License - 与 ccLoad 项目保持一致
