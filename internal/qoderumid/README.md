# Qoder UMID 组件

本包独立实现官方 `runtime-info` 组件的发现、安装和调用，不包含第三方网关代码，也不重新实现组件内部的设备风控算法。

- 复用 `${DataDir}/qoder-umid/runtime-info`，其次查找官方 Qoder / Qoder CN 客户端的 `resources/umid/runtime-info`（Windows 为 `.exe`），或用户目录 `.qoder/.bin/umid-*/runtime-info`、`.qoder-cn/.bin/umid-*/runtime-info`。
- 缺失时 Linux 与 macOS（amd64 / arm64）自动安装。唯一网络来源为官方 npm registry 的 [`@qoder-ai/qodercli`](https://registry.npmjs.org/@qoder-ai%2fqodercli/latest)：读取 latest metadata，用其 SHA-512 integrity 严格验证官方 tarball，再用 Go 标准库读取 `package/bundle/qodercli.js` 中匹配本机平台（Linux 为 ELF，macOS 为 Mach-O）与架构的 base64 组件。没有镜像、shell、npm 安装命令或未校验回退。Windows 组件需配套 `sgsdk.dll`，不自动安装，缺组件时需安装官方客户端或官方 CLI。
- 下载包最大 128 MiB，解压数据最多 512 MiB，bundle 最大 256 MiB，组件最大 64 MiB；只在数据目录临时写入经过校验的 ELF / Mach-O，原子发布。包内路径、链接、重定向与源站均受限制。
- 安装有 3 分钟总超时；组件执行使用 `exec.CommandContext`，默认 25 秒超时，stdout 在读取时限制到 1 MiB。国际版客户端传环境号 `3`，CLI / 下载组件传 `4`。Windows / macOS 添加 `--account-stdin` 并通过 stdin 写入 `{"account":"<uid>"}`；Linux 只传环境号。首行 JSON 的 `machineToken`、`machineCode`、`machineType` 映射到对应的 `Cosy-Machine*` 请求头，并按 region + UID 缓存 50 分钟。
- 国际版 region 为 `global`；`cn` / `china` 不需要本包的风控身份头。状态接口不返回组件路径、UID、token 或组件原始输出。

组件是官方原生可执行文件；Linux 上需要对应架构、可执行的数据卷及组件要求的系统运行库。已检查的 1.1.73 使用 glibc 动态链接器（amd64 为 `/lib64/ld-linux-x86-64.so.2`，arm64 为 `/lib/ld-linux-aarch64.so.1`），依赖 `libstdc++.so.6`、`libgcc_s.so.1` 和 glibc 系列运行库；纯 musl / Alpine 环境无法直接执行该组件。容器应挂载持久化 DataDir，并允许访问 `registry.npmjs.org:443`。自动下载本身不会执行二进制；只有 `Headers` 会执行。安装成功只证明完整性、可执行格式和架构有效，不表示宿主运行库已经满足。macOS 组件是官方 Developer ID 签名的 Mach-O，只依赖系统框架。执行失败会明确返回错误。

2026-10-19 只读检查官方 npm `qodercli@1.1.73` 的 SHA-512、tar 内容和内嵌 ELF；未运行下载的组件。测试使用合成 ELF 安装包和本地 Go 测试程序作为假组件，覆盖校验拒绝、安装并发、账号缓存隔离、输出限制和进程取消。
