# Kubernetes Runtime 实验能力

## 1. 当前状态

本能力固定标记为 `experimental`。控制面会注册 Provider 状态、资源观察和预检
API，但默认关闭，并且不提供 Apply、生命周期、Console、Mod 发布或备份恢复 API。
当前交付用于验证 Kubernetes 运行模型，不能据此宣称已经支持生产服。

代码边界：

- `internal/kubernetesruntime`：Provider、只读 Kubernetes REST adapter、预检、
  类型化资源和 Client 边界。
- `deploy/kubernetes`：独立 namespace、Provider ServiceAccount、无权限的
  Shard ServiceAccount 和 namespace Role。
- 不提供 `kubectl` Shell adapter，不接受任意 YAML、镜像、命令、路径、挂载、
  Node selector、StorageClass 或 hostPort。

控制面始终注册以下认证接口，便于 UI 稳定展示禁用或配置错误状态：

```text
GET  /api/v2/runtime-providers/kubernetes
POST /api/v2/runtime-providers/kubernetes/{providerId}/shards/observe
POST /api/v2/runtime-providers/kubernetes/{providerId}/shards/preflight
```

不存在 Apply 路由。`preflight` 即使生成类型化 mutation preview，也始终返回
`applyAllowed=false`。REST adapter 只读取 StatefulSet、Pod、PVC、内部/公网 Service
和 NetworkPolicy；连接只接受 HTTPS origin、绝对路径 CA 与 bearer token 文件，
禁用代理并限制超时、Token、响应和 Provider 配置大小。

默认状态由下列环境变量控制：

```sh
export DST_ADMIN_KUBERNETES_EXPERIMENTAL_ENABLED=true
export DST_ADMIN_KUBERNETES_PROVIDER_CONFIG=/absolute/path/provider.json
```

可修改的完整配置见 `deploy/kubernetes/provider.example.json`。其中
`minimumLeaseRemaining` 和 `maximumObservationAge` 当前按 Go `time.Duration` 的纳秒值
编码，例如 30 秒为 `30000000000`。能力 attestation 在完成对应集群实测前必须保持
`false`。配置无效、CA/Token 不可读或 Kubernetes API 离线只会降级此 Provider，
不会阻止本机和 Agent Runtime 启动。

## 2. 资源模型

一个 Shard 对应一个 StatefulSet、一个独立 PVC、一个内部 ClusterIP Service 和一个
ingress NetworkPolicy；启用外部暴露时再增加一个 public Service。资源名由
Provider/Room/World ID 的摘要稳定生成，所有资源必须同时
具有下列受管标签：

```text
app.kubernetes.io/managed-by=dst-admin
app.kubernetes.io/component=dst-shard
runtime.dst-admin.io/provider-id=<digest>
runtime.dst-admin.io/room-id=<digest>
runtime.dst-admin.io/world-id=<digest>
```

原始 ID 只进入固定 annotation 和 exec 参数数组，不参与 Shell，也不会作为路径。
Cluster 与 Shard 目录仅允许安全的单段名称。Provider 固定 namespace、运行镜像、
Shard ServiceAccount、Storage profile 和 Compute profile；请求只能引用 profile ID。

`provision` 始终生成 `replicas=0`。`stop` 只缩容 StatefulSet，不创建、更新或删除
PVC、Service 和 NetworkPolicy；Client 必须使用 StatefulSet `scale` subresource，
不能在停服时替换 Pod template。只有 `start` 在全部门禁通过后才生成
`replicas=1`。StatefulSet 使用 `OnDelete` 更新策略，但 StatefulSet 控制器仍可能在
Pod 丢失后自动重建，所以 `OnDelete` 本身不是 fencing。

`stop` 仍要求新鲜的 Shard observation、有效 lease/fencing 和精确 Pod/PVC ownership，
但不依赖 CPU telemetry、Storage/Compute profile 或外部网络能力，避免无关探针过期
阻止安全停服。

## 3. 强制启动门禁

Provider 只有完成集群实测后才能声明以下能力：

1. lease/fencing admission：创建 Pod 前原子校验 operation、lease、拓扑 revision
   和单调 fencing token。
2. lease-aware runtime supervisor：DST 启动和持续运行受有效 lease 约束，不能把
   Pod Running 当作游戏 ready。镜像必须在初始 expiry 到达前停止 DST，并通过经过
   验证的固定续租通道获取更新；只把 expiry 写进 Pod 模板但不执行停止不算支持。
3. Pod UID ownership gate：旧 Pod 的 UID、终止状态和 resourceVersion 必须与计划
   一致；Terminating/Unknown 或未确认删除的旧 Pod 阻止新启动。
4. PVC UID ownership gate：PVC UID、StorageClass、受管标签、访问模式和 Retain
   语义必须一致，禁止采用未知卷或 RWX 共享卷。
5. enforcing NetworkPolicy：集群 CNI 必须真实执行 NetworkPolicy，而不只是接受对象。

预检拒绝较低 fencing token；相同 token 只能由相同 lease 和相同 operation 做幂等
重放。Pod/PVC 存在时请求必须提供完全一致的 UID，不存在时不得携带旧 UID。
`Client.Apply` 还必须在写入时原子执行 UID 和 resourceVersion 前置条件，避免
Observe 与 Apply 之间的竞态。整体资源和 CPU observation 还必须落在 Provider 固定
的新鲜度窗口内，过期或明显时钟偏移都会阻止计划。

## 4. CPU、网络和存储

### CPU

- `shared` 只接受 Provider 资源边界内的 millicore request/limit，并从新鲜 CPU
  observation 扣除至少一个系统物理核心；observation 必须与所选 Compute profile
  精确对应。
- `exclusive` 要求 CPU Manager `static`、整物理核策略和已知 SMT 拓扑；CPU request
  与 limit 必须等于一个物理核心的全部 logical CPU，memory request/limit 也必须
  相等，确保 Guaranteed QoS。
- 请求不能指定 logical CPU ID。Node selector 只能来自受信 Compute profile。

### 网络

请求必须完整提供四种类型化 UDP 端口并保证互不重复：DST server、Cluster Master、
Steam authentication 和 Steam master server。Secondary 只在自己的 Pod/Service 上
绑定除 Cluster Master 外的三种本地端口，并通过稳定的 Master Service DNS 连接
Cluster Master 端口。

暴露策略只有 `internal`、`node_port` 和 `load_balancer`。NodePort 必须显式给出、
落在 Provider 固定范围且无重复；LoadBalancer 不分配隐藏 NodePort。两种外部策略
都必须先验证 UDP 转发、Steam 公布端点和外部连接行为，并提供已验证的 advertise
address。public Service 只包含 DST server 和两个 Steam 端口，Cluster Master 端口
始终只在内部 Service 上提供。`hostPort` 不受支持。

NetworkPolicy 对玩家相关 UDP 端口开放 ingress；Cluster Master 端口只允许同一
namespace、同一 Provider 和同一 Room 的受管 Shard Pod 访问。当前策略不限制
egress，避免在 Steam、DNS 和版本更新目的地址尚未形成可验证清单前制造假可用。

### 存储

Storage profile 必须由管理员预注册，并验证动态供应、`Retain`、容量范围及 RWO 或
RWOP。每个 Shard 使用独立 PVC 和唯一 `/data` 挂载，不设置会级联删除 PVC 的
ownerReference。已有 PVC 必须是 `Bound`，容量不能缩小。

CSI snapshot capability 只表示可能优化单卷复制，不能证明跨 Shard 的 Room 备份
一致性。完整备份仍必须经过 DST 保存屏障、各 Shard manifest 和集中校验；snapshot
不可用时使用通用 manifest/上传 fallback。

## 5. 镜像、凭证和权限

Provider 只接受 `@sha256:` 固定摘要镜像。生成的容器使用固定 supervisor 路径和
固定参数集合，以非 root 用户运行，root filesystem 只读，禁止提权和 privileged，
丢弃全部 Linux capabilities，并关闭 ServiceAccount token 自动挂载。

Shard 通过稳定名称引用由管理员预先创建的 Room Secret，且只读取固定的
`cluster-token` 和 `cluster-key` 两个 key，不使用 `envFrom`。Kubernetes Provider Role
无 Secret read/create/update 权限；不要把 Token 或 cluster key 放进 ConfigMap。
当前 `deploy/docker/Dockerfile.dst-runtime` 及其 shell supervisor 尚未实现本文要求的
lease-aware admission/runtime 契约，不能直接作为已验证的 Kubernetes Runtime 镜像。

RBAC 只能限制 namespace 和 resource/verb，不能按 label 限制写入。因此生产化前
必须增加 admission policy/webhook，拒绝越过受管标签、UID、resourceVersion、lease
和 fencing 前置条件的 mutation。不得给 Provider Pod exec/attach、Job、Node、drain、
hostPath、hostPID、privileged 或 cluster-admin 权限。

## 6. 已知限制

- 当前 API Client adapter 仅观察并生成只读预检计划，没有 Apply 实现或公网上线能力。
- 核心 Kubernetes API 无法证明物理核心和 SMT 分配，REST observation 会明确把 CPU
  标记为 stale，因此 `provision/start` 会保持阻断，直到接入可信节点观察 adapter。
- namespace Role 无法读取 cluster-scoped PV reclaim policy；已有 PVC 的 Retain 证明
  不会被猜测，相关启动会保持阻断。
- 没有可用的 lease-aware supervisor 镜像和 admission 实现。
- 没有实现 console transport、日志 continuation、保存回执和操作证据。
- 没有实现 Secret delivery、Config 发布、Mod 分发和 DST 二进制/镜像发布流程。
- 最小权限 Role 不含 delete；从外部暴露切回 internal 时，必须先通过未来的 UID
  ownership cleanup 流程移除 public Service，当前预检会阻止静默遗留公网入口。
- 没有实现跨 Shard 保存屏障、CSI snapshot adapter 或恢复流程。
- 未验证 Klei 的公网端口公布、NodePort/LB 源地址和 Service DNS `master_ip` 行为。
- 未对任何 Kubernetes/CSI/CNI 组合完成故障注入，因此不能开启生产声明。
- 当前 SQLite 控制面必须保持单副本运行；本方案不提供、也不宣称控制面 HA。

## 7. 进入生产前的验收

至少在两个 Kubernetes/CSI 组合上自动验证：

1. Master/Secondary 分节点注册、断线恢复和稳定 DNS。
2. NodePort、UDP LoadBalancer、Steam 列表和公网直连。
3. CPU Manager static 可用/不可用、SMT sibling 完整分配和系统核心预留。
4. Worker NotReady、网络分区、旧 Pod Terminating/Unknown、强制删除和 UID 变化。
5. PVC 重挂载、错误 StorageClass/RWX/Delete 策略、卷丢失和数据保留。
6. lease 过期、较低/equal-conflict fencing、控制面重启和 Apply 竞态。
7. readiness 只在世界加载和 Shard 注册完成后成功；console 不健康时阻止热保存和
   危险命令。
8. 一致性备份部分失败、snapshot 部分失败、manifest fallback、整套恢复和完整性
   校验。
9. Mod/版本分阶段发布失败和回滚。
10. Provider 权限审计，确认无法读 Secret、exec/attach Pod、创建 Job、修改 Node 或
    操作其他 namespace。

在上述矩阵通过、Client/admission/supervisor 均交付前，功能状态必须持续显示
“实验能力”，默认不可供普通用户创建运行中的 Shard。

## 8. Debian 12 / kind 端到端验证

以下步骤只验证默认关闭、配置降级、只读观察和门禁输出，不会启动 DST：

1. 使用 `kubectl apply -k deploy/kubernetes` 创建隔离 namespace 和只读身份。
2. 为 `dst-admin-kubernetes-provider` 签发短期 Token，并把 Token 与集群 CA 以仅控制面
   用户可读的权限写入配置中列出的绝对路径。
3. 复制并修改 `deploy/kubernetes/provider.example.json`；镜像必须换成真实、固定
   `@sha256:` 摘要，但所有未经实测的 capability 保持 `false`。
4. 设置两个环境变量并以单副本启动 API，读取 Provider 状态。
5. 对一个测试 Room/World 调用 observe，再调用 preflight；确认 start 被
   lease supervisor/admission、网络、CPU 或存储证明门禁阻止。
6. 确认不存在 Apply endpoint，并在配置路径错误、CA 错误和 API Server 离线时验证
   本机/Agent 管理仍正常。

示例请求：

```sh
curl -fsS -b cookie.txt http://127.0.0.1:8000/api/v2/runtime-providers/kubernetes

curl -fsS -b cookie.txt \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $DST_CSRF_TOKEN" \
  -H "Idempotency-Key: k8s-observe-0001" \
  -d '{"roomId":"room-1","worldId":"master"}' \
  http://127.0.0.1:8000/api/v2/runtime-providers/kubernetes/k8s-lab/shards/observe
```

正式可用前仍需在至少两个 Kubernetes/CSI 组合上完成 admission、lease-aware
supervisor、CPU/PV 可信观察、CNI NetworkPolicy、UDP/Steam、Console、Mod 分发、
一致性备份恢复和故障注入验证。
