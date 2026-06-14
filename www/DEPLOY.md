# ccLoad 介绍网站部署指南

这是 ccLoad 项目的独立介绍网站，可以部署到任何静态 Web 服务器。

## 📁 目录说明

`www/` 目录是一个**完全独立**的静态网站，包含：

- ✅ HTML/CSS/JS（无框架依赖）
- ✅ 中英双语支持
- ✅ 主题切换（light/dark/system）
- ✅ 响应式设计
- ✅ 可直接复制到任何 Web 服务器

## 🚀 快速部署

### 方法一：使用 Makefile（开发环境）

```bash
# 1. 设置网站（复制共享资源）
make www-setup

# 2. 本地预览（Python 简易服务器）
make www-run

# 访问 http://localhost:8888/
```

### 方法二：复制到 Nginx

```bash
# 1. 设置网站
make www-setup

# 2. 复制到 Nginx 目录
sudo cp -r www /usr/share/nginx/html/

# 3. 配置 Nginx（示例）
# 在 /etc/nginx/sites-available/default 或你的站点配置中添加：

server {
    listen 80;
    server_name your-domain.com;
    root /usr/share/nginx/html/www;
    index index.html;

    location / {
        try_files $uri $uri/ =404;
    }

    # 缓存策略
    location ~* \.(css|js|jpg|jpeg|png|gif|svg|ico|woff|woff2|ttf|eot)$ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }

    location ~* \.html$ {
        expires -1;
        add_header Cache-Control "no-cache, must-revalidate";
    }
}

# 4. 重启 Nginx
sudo nginx -t && sudo systemctl reload nginx
```

### 方法三：复制到 Apache

```bash
# 1. 设置网站
make www-setup

# 2. 复制到 Apache 目录
sudo cp -r www /var/www/html/

# 3. 创建 .htaccess（可选）
cat > www/.htaccess << 'EOF'
# 启用 Gzip 压缩
<IfModule mod_deflate.c>
    AddOutputFilterByType DEFLATE text/html text/css text/javascript application/javascript
</IfModule>

# 缓存策略
<IfModule mod_expires.c>
    ExpiresActive On
    ExpiresByType text/html "access plus 0 seconds"
    ExpiresByType text/css "access plus 1 year"
    ExpiresByType application/javascript "access plus 1 year"
    ExpiresByType image/svg+xml "access plus 1 year"
    ExpiresByType image/x-icon "access plus 1 year"
</IfModule>
EOF

# 4. 重启 Apache
sudo systemctl reload apache2
```

### 方法四：部署到 GitHub Pages

```bash
# 1. 设置网站
make www-setup

# 2. 创建独立仓库
cd /tmp
git clone https://github.com/your-username/ccload-website.git
cd ccload-website

# 3. 复制文件
cp -r /path/to/ccLoad/www/* .

# 4. 推送到 GitHub
git add .
git commit -m "Initial commit"
git push

# 5. 在 GitHub 仓库设置中启用 Pages
# Settings -> Pages -> Source: main branch / root
```

### 方法五：部署到 Netlify/Vercel

```bash
# 1. 设置网站
make www-setup

# 2. 安装 CLI（选择其一）
npm install -g netlify-cli
# 或
npm install -g vercel

# 3. 部署
cd www
netlify deploy --prod
# 或
vercel --prod
```

## 🔧 手动设置（不使用 Makefile）

如果你不在 ccLoad 项目环境中，可以手动设置：

```bash
cd www

# 复制共享资源（从 ccLoad 的 web 目录）
cp ../web/assets/css/styles.css assets/css/
cp ../web/assets/js/i18n.js assets/js/
cp ../web/assets/js/theme-init.js assets/js/
cp ../web/favicon.* ../web/apple-touch-icon.png .

# 现在 www 目录完全独立，可以复制到任何地方
```

## 📝 目录结构

```
www/
├── index.html              # 首页
├── install.html            # 安装指南
├── config.html             # 配置文档
├── usage.html              # 使用指南
├── feedback.html           # 反馈渠道
├── favicon.svg/ico         # 图标
├── apple-touch-icon.png
├── assets/
│   ├── css/
│   │   ├── styles.css      # 共享设计系统（复制自 web）
│   │   └── www.css         # 网站专用样式
│   ├── js/
│   │   ├── i18n.js         # 国际化系统（复制自 web）
│   │   ├── theme-init.js   # 主题初始化（复制自 web）
│   │   ├── nav.js          # 导航组件
│   │   └── www.js          # 交互逻辑
│   └── locales/
│       ├── zh-CN.js        # 中文语言包
│       └── en.js           # 英文语言包
└── .gitignore              # 忽略复制的文件
```

## ⚙️ 配置说明

### 修改端口（本地预览）

```bash
make www-run WWW_PORT=9000
```

### 自定义域名

在你的 DNS 提供商处添加 A 记录或 CNAME 记录指向你的服务器。

### HTTPS 配置

推荐使用 Let's Encrypt：

```bash
# Nginx
sudo certbot --nginx -d your-domain.com

# Apache
sudo certbot --apache -d your-domain.com
```

## 🔍 验证部署

访问以下 URL 确认部署成功：

- 首页：`http://your-domain.com/`
- CSS：`http://your-domain.com/assets/css/www.css`
- JS：`http://your-domain.com/assets/js/nav.js`
- 语言包：`http://your-domain.com/assets/locales/zh-CN.js`

## 📊 浏览器兼容性

- Chrome/Edge 90+
- Firefox 88+
- Safari 14+
- 移动端浏览器

## 🐛 故障排除

### 样式未加载

确认 `make www-setup` 已执行，检查 `assets/css/styles.css` 是否存在。

### 语言切换不工作

检查浏览器控制台是否有 JS 错误，确认 `assets/js/i18n.js` 已加载。

### 图标未显示

确认 `favicon.svg` 和 `favicon.ico` 已复制到 www 根目录。

### 相对路径问题

确保 HTML 中的资源引用使用相对路径（不以 `/` 开头）。

## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

## 📄 License

MIT License - 与 ccLoad 项目保持一致
