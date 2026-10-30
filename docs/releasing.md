# 发布与镜像同步

普通分支提交和 Pull Request 执行后端 CI，不生成正式版本。推送 `vX.Y.Z`
标签才发布正式版；`vX.Y.Z-rc.N` 发布预览版。发布前请把待测功能保留在
独立分支或 worktree，确认标签对应的提交和前端版本均为本次发布范围。

## 自动发布

1. 准备 `docs/releases/vX.Y.Z.md`，提交代码并推送到 `master`。
2. 创建并推送对应的版本标签。
3. 等待 `Package` 成功，核对 Release、在线更新索引以及镜像同步结果。

Release 同时提供官方 AstrBot 伴随助手与播报插件 ZIP，以及各自的 SHA256 文件；安装与升级由机器人管理者单独执行。

发布固定前端提交号，只构建一次页面；原生包和容器复用经过摘要校验的
前端产物。原生包构建与后端检查并行，Docker 仅依赖 Linux 原生包，
不等待 macOS 检查。正式 Release 仍要求所有平台和镜像检查通过。

后端检查优先复用最近 24 小时内、同一提交的 `master` CI；正在执行时等待
其结果，失败会阻止发布。没有可复用结果时执行完整检查。前端测试和契约
核验仍在本次发布中执行。

日常发布的启动观察为 30 秒。在线升级、自动恢复、Agent 重连、配置和数据
保留、只读根文件系统等功能检查仍完整执行。每周一执行 180 秒观察的
非发布检查，也可以手动运行 `Package` 并选择 `extended_checks`。
只有手动勾选 `publish_images` 才会推送预览镜像。

## 从本机同步阿里云

默认由 GitHub 自动同步。需要改成本机同步时，将仓库 Actions 变量
`ALIYUN_PUBLISH_MODE` 设为 `local`；恢复自动同步设为 `github` 或删除变量。
本机模式下，GitHub 发布成功只代表 GitHub 和已配置的 Docker Hub 已完成，
阿里云需要执行下列命令后才完成同步。维护机器离线时不会自动补传。

先使用有仓库推送权限的阿里云镜像账号登录，密码通过标准输入传入：

```bash
docker login registry.cn-hangzhou.aliyuncs.com
node deploy/scripts/sync-aliyun.mjs --version vX.Y.Z --dry-run
node deploy/scripts/sync-aliyun.mjs --version vX.Y.Z --promote-latest
```

脚本读取本机 Docker 凭据，默认从 Docker Hub 复制四类已经构建的镜像。
无需本机 Docker 守护进程，不重新构建，不根据 Mac/Windows 的 CPU 架构
筛选镜像；保留完整镜像清单和摘要。优先使用已安装的 `crane`，缺失时通过
Go 运行固定版本 `crane v0.20.6`。可提前安装以减少首次执行的等待：

```bash
go install github.com/google/go-containerregistry/cmd/crane@v0.20.6
```

每次最多同步两个镜像，每个镜像最多并行两个传输任务；已有层会复用。
每个版本标签复制完成后校验目标摘要。仅当全部四个镜像验证通过，且请求
版本仍为来源的 `latest` 时，`--promote-latest` 才更新阿里云稳定标签。
同步历史版本时省略该选项；预览版不能更新稳定标签。

可以指定 `--source ghcr`，此时本机还需具备 GHCR 读取权限。其他命名空间
使用 `--target registry.cn-hangzhou.aliyuncs.com/命名空间/dst-admin-go`，
也可以设置 `DST_ADMIN_ALIYUN_REGISTRY`、`DST_ADMIN_ALIYUN_NAMESPACE`。

本机是否更快取决于新镜像层的实际下载和上传带宽。仅同步已经存在的层只能
验证鉴权、清单复制和摘要，不能用于估算下一个版本的新层上传速度。
