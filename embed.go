package main

import "embed"

// WebFS 嵌入 web 目录的静态资源
// all: 前缀确保包含以 . 开头的文件（如 .htaccess），但会自动忽略 .git 等
//
//go:embed all:web
var WebFS embed.FS
