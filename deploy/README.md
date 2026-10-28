# 反代上传超时与服务 stderr 轮转

这些文件需要在部署时安装；仅升级网关二进制不会改变既有 Nginx 或 OpenRC 配置。
本次 PR 不重启、不修改现有生产实例，不清理已有账本和日志。

## Nginx 上传超时

将 `nginx/workbuddy-api-upload.conf` 安装到 `/etc/nginx/snippets/workbuddy-api-upload.conf`，
在每个提供 API 的 `server {}` 内加入：

```nginx
include /etc/nginx/snippets/workbuddy-api-upload.conf;
```

已有同名 `client_body_timeout` 时修改原值，避免重复声明。检查更具体的 `location {}` 是否覆盖了它。
入口中转与源站 Nginx 都要配置；仅修改其中一跳无法延长另一跳的上传等待。
保持已有 TLS、监听端口、`client_max_body_size`、关闭请求/响应缓冲以及禁用 POST 重放的配置。

```sh
nginx -t
nginx -T 2>&1 | grep -n -E 'server_name|client_body_timeout|proxy_request_buffering'
# 配置检查通过后，按该机器的服务管理方式执行平滑 reload。
```

`900s` 与网关默认 `apiReadTimeoutMs: 900000` 数值一致。
[Nginx 此值限制两次请求体读取之间的空闲间隔](https://nginx.org/en/docs/http/ngx_http_core_module.html#client_body_timeout)，
并非整个上传的总耗时。网关仍执行自己的总读取期限；CDN 也有独立限制。
`unexpected_eof` 也可能来自客户端取消或链路中断，不能仅凭该日志认定是 Nginx 超时。
部署后用合成上传在两次发送间暂停超过 60 秒、低于 900 秒，确认各跳不再提前关闭；不要使用真实提示词或收费接口。

## OpenRC 服务 stderr

应用的 `gateway-*.log` 预算不会接管 OpenRC 打开的 stderr 文件。
`logrotate/workbuddy-service-error` 仅处理 `/var/log/workbuddy-gateway/service-error.log`，
不匹配应用日志、访问日志、Key 文件、WAL 或快照。策略为每日轮转，检查时超过 8 MiB 也轮转，保留 7 份并压缩。

以 root 安装（系统须已有 logrotate；Alpine 可用 `apk add logrotate`）：

```sh
install -d -m 0755 /etc/workbuddy-gateway /var/lib/logrotate
install -m 0644 deploy/logrotate/workbuddy-service-error /etc/workbuddy-gateway/service-error.logrotate
# Alpine / BusyBox crond：
install -m 0755 deploy/cron.hourly/workbuddy-service-error /etc/periodic/hourly/workbuddy-service-error
# Debian 等使用 /etc/cron.hourly 的系统改用该目录，只安装一处。
/usr/sbin/logrotate --debug --state /var/lib/logrotate/workbuddy-service-error.status /etc/workbuddy-gateway/service-error.logrotate
```

确认系统的小时任务调度正在运行，且没有其他 logrotate 规则重复匹配该文件。
脚本使用独立状态文件及其锁，不与系统的全局轮转状态竞争；不要同时将这份规则放入 `/etc/logrotate.d/`。
`maxsize` 只在轮转程序执行时检查，不是实时硬上限，突发大量日志仍可能暂时超过 8 MiB。

OpenRC stderr 不能由应用日志器重新打开，因此使用 `copytruncate` 保留原 inode，无需重启模型请求。
[logrotate 对此模式说明有很短的复制/截断窗口](https://github.com/logrotate/logrotate/blob/main/logrotate.8.in)，
可能丢失同时追加的少量诊断行；这里不用于消费账本。首次处理大文件前应留有复制空间。
验证时用独立临时文件、保持追加 fd 打开后强制轮转，再写一行，检查 inode 保留且新行落在当前文件。
生产首次轮转后检查文件大小、归档和持续追加，不执行手工删日志或清空账本。
