# 测试并行化

调整测试并行度时读取；普通功能修改不要求顺带并行化测试。

`make race-fast` 运行高价值 race 子集，`make race` 运行全量；可用 `RACE_P` / `RACE_PARALLEL` 调整并行度。

- 积极 `t.Parallel()`:并行测试必须独立 Store/Server、随机监听端口、局部 mock,不共享可变 fixture;调用 `t.Setenv`/`t.Chdir`,或修改进程环境、工作目录、`http.DefaultTransport`、全局模型目录、包级 session/cache、全局 goroutine 计数的测试必须串行
- 优先并行化含 sleep、deadline、轮询、异步日志等待的高耗时测试;不为凑并行数改纯解析微测试
- 并行化前后同命令计时(`/usr/bin/time -p go test -tags sonic -count=1 ./internal/app`),收益属噪声就撤销;新增/调整并行测试后跑 `go test -race -tags sonic -count=1 -shuffle=on ./internal/app`
