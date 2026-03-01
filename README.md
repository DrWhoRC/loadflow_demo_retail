# LoadFlow Retail Demo — 电商数据管线全场景演示

> 基于 [LoadFlow](https://github.com/DrWhoRC/loadflow) 框架构建的真实场景 Demo，使用 Kaggle [Online Retail II](https://www.kaggle.com/datasets/mashlyn/online-retail-ii-uci) 数据集（~107 万行），将 CSV 数据经过三条差异化 Stream 写入 MySQL，完整展示 **WRR 加权轮询**、**粘性路由**、**StripedPool 强有序**三种策略，以及 **pressure\_rebalance 动态重均衡**机制。全程通过 Prometheus + Grafana 实时可视化。

---
## Overall View in Grafana:
![Overall View in Grafana](docs/images/overall_view.png)

## 目录

- [架构概览](#架构概览)
- [三条数据流设计](#三条数据流设计)
  - [Stream 1 — 商品目录批量导入（Keyless WRR）](#stream-1--商品目录批量导入keyless-wrr)
  - [Stream 2 — 客户交易聚合（Sticky 粘性路由）](#stream-2--客户交易聚合sticky-粘性路由)
  - [Stream 3 — 订单状态机（StripedPool 强有序）](#stream-3--订单状态机stripedpool-强有序)
- [pressure_rebalance 动态重均衡](#pressure_rebalance-动态重均衡)
- [项目结构](#项目结构)
- [快速开始](#快速开始)
  - [前置条件](#前置条件)
  - [1. 启动 MySQL](#1-启动-mysql)
  - [2. 启动 Prometheus + Grafana](#2-启动-prometheus--grafana)
  - [3. 运行 Demo](#3-运行-demo)
  - [4. 查看 Grafana 仪表盘](#4-查看-grafana-仪表盘)
- [配置说明](#配置说明)
- [运行结果](#运行结果)
- [Grafana 仪表盘详解](#grafana-仪表盘详解)
  - [Panel 1: Routing Weights — 路由权重变化](#panel-1-routing-weights--路由权重变化)
  - [Panel 2: Queue Depth — 队列深度](#panel-2-queue-depth--队列深度)
  - [Panel 3: Processing Rate — 处理速率](#panel-3-processing-rate--处理速率)
  - [Panel 4: Rebalance Events — 重均衡事件](#panel-4-rebalance-events--重均衡事件)
  - [Panel 5: Sticky Violations — 粘性路由违规](#panel-5-sticky-violations--粘性路由违规)
  - [Panel 6: Orders State Machine — 订单状态机](#panel-6-orders-state-machine--订单状态机)
  - [Panel 7: MySQL Write Latency — 数据库写入延迟](#panel-7-mysql-write-latency--数据库写入延迟)
  - [Panel 8: Stream Throughput — 流吞吐量](#panel-8-stream-throughput--流吞吐量)
  - [Panel 9: Routing Distribution — 路由分发分布](#panel-9-routing-distribution--路由分发分布)
- [关键设计决策](#关键设计决策)
- [技术栈](#技术栈)

---

## 架构概览

```
                   ┌─────────────────────────────────────────────────────────┐
                   │                     CSV Data Source                     │
                   │            Kaggle Online Retail II (50,000 rows)        │
                   └────────────┬──────────────┬──────────────┬──────────────┘
                                │              │              │
                    ┌───────────▼──┐  ┌────────▼───────┐  ┌──▼──────────────┐
                    │  Stream 1    │  │   Stream 2     │  │   Stream 3      │
                    │  Products    │  │   Customers    │  │   Orders        │
                    │  300 msg/s   │  │   200 msg/s    │  │   100 order/s   │
                    │  Keyless WRR │  │   Sticky Key   │  │   StripedPool   │
                    └──────┬───────┘  └───────┬────────┘  └───────┬─────────┘
                           │                  │                   │
                  ┌────────▼──────────────────▼────────┐   ┌──────▼──────────┐
                  │       WeightedRR Router            │   │  StripedPool    │
                  │  InstrumentedRouter (指标采集)       │   │  8 stripes      │
                  │  + KeyTracker (粘性违规检测)         │   │  per-key 串行    │
                  └─┬──────────────┬───────────────┬───┘   └──────┬──────────┘
                    │              │               │              │
              ┌─────▼────┐  ┌─────▼──────┐  ┌─────▼────┐  ┌─────▼────┐
              │pool_fast  │  │pool_medium │  │pool_slow │  │ dbFast   │
              │ 8 workers │  │ 4 workers  │  │ 2 workers│  │(保序优先) │
              │ queue=2048│  │ queue=1024 │  │ queue=512│  └──────────┘
              └─────┬─────┘  └─────┬──────┘  └────┬─────┘
                    │              │               │
              ┌─────▼─────┐  ┌────▼──────┐  ┌─────▼─────┐
              │ dbFast     │  │ dbMedium  │  │ dbSlow    │
              │ MaxConn=20 │  │ MaxConn=5 │  │ MaxConn=2 │
              └─────┬──────┘  └────┬──────┘  └─────┬─────┘
                    │              │               │
                    └──────────────┼───────────────┘
                                   │
                          ┌────────▼────────┐
                          │     MySQL       │
                          │ loadflow_demo   │
                          └─────────────────┘
```

**Scheduler（调度器）** 每秒采样各 pool 的 `PoolStat.Pressure`（= QueueDepth / ProcessRate），当最大压力池与最小压力池的差值超过阈值时，触发 `pressure_rebalance` 策略，**从高压力 pool 偷权重给低压力 pool**，实现动态负载均衡。

---

## 三条数据流设计

### Stream 1 — 商品目录批量导入（Keyless WRR）

| 属性 | 值 |
|------|------|
| 数据来源 | CSV 全量记录（50,000 行） |
| 路由策略 | Keyless WRR（加权轮询，无 key） |
| 初始权重 | `[1, 1, 8]` → 80% 压给 `pool_slow` |
| 发射速率 | 300 msg/s |
| Handler | `ProductHandler` — 反序列化 → GORM `INSERT` → MySQL `products` 表 |
| 重均衡 | ✅ 启用 `pressure_rebalance`，cooldown=3s，maxStep=2 |

**设计意图**：纯吞吐场景，不关心消息顺序。初始权重故意不均衡，将绝大部分流量导向 worker 最少的 `pool_slow`，使其快速堆积队列，从而触发 `pressure_rebalance` 将权重逐步转移到 `pool_fast`。

### Stream 2 — 客户交易聚合（Sticky 粘性路由）

| 属性 | 值 |
|------|------|
| 数据来源 | 有 CustomerID 的记录（35,093 行） |
| 路由策略 | Sticky（以 `CustomerID` 为 key，粘性到 pool） |
| 初始权重 | `[1, 1, 8]` |
| 发射速率 | 200 msg/s |
| Handler | `CustomerHandler` — 内存聚合 per-customer 累计金额 → GORM `INSERT` → MySQL `customer_transactions` 表 |
| 重均衡 | ✅ 启用，cooldown=5s，maxStep=1 |

**设计意图**：展示粘性路由的业务价值 — 同一客户的交易始终路由到同一 pool，使得 handler 中的 per-customer 内存缓存（`custTotals`）天然具有局部性，并发冲突概率极低。当 `pressure_rebalance` 调整权重时，部分 key 被迁移到新 pool，产生 **sticky violation**（粘性违规），在 Grafana 上清晰可见。这展示了 **吞吐 vs. 亲和性 的 trade-off**。

### Stream 3 — 订单状态机（StripedPool 强有序）

| 属性 | 值 |
|------|------|
| 数据来源 | 按 InvoiceNo 聚合的订单事件（1,909 笔） |
| 路由策略 | `StripedPool.SubmitWithKey`（以 `InvoiceNo` 为 key，同一订单在同一 stripe 串行执行） |
| Stripe 数 | 8 |
| 队列大小 | 每 Stripe 256 |
| Handler | `OrderHandler.ProcessOrder` — 状态机：`created → processing → completed` |
| 重均衡 | ❌ 不启用（保序优先） |

**设计意图**：证明强有序的业务价值。每笔订单经历三步状态机：

1. **CREATE** — 创建订单记录（`status = 'created'`）
2. **INSERT ITEMS** — 逐个添加明细行，第一个 item 后状态变为 `processing`
3. **COMPLETE** — 更新为 `completed`，写入汇总金额

如果这三步乱序执行（比如 COMPLETE 在 CREATE 之前），SQL `UPDATE ... WHERE status = 'processing'` 会匹配不到行而失败。StripedPool 保证同一 `InvoiceNo` 的所有闭包在同一 stripe 中**串行**执行，1,909 笔订单全部成功，0 错误。

---

## pressure_rebalance 动态重均衡

这是 Demo 的核心看点。算法流程：

```
每 tick(1s) 采样:
  ┌─────────────────────────────────────────────────────────┐
  │ 对每个 stream，计算各 pool 的 Pressure:                    │
  │   Pressure = QueueDepth / ProcessRate                    │
  │                                                          │
  │ 找到 maxPressure pool 和 minPressure pool                 │
  │ delta = maxPressure - minPressure                         │
  │                                                          │
  │ if delta >= minPressureDelta (3.0):                       │
  │   deltaW = min(maxStep, calculated_step)                  │
  │   从 maxPressure pool 偷 deltaW 权重 → 给 minPressure pool │
  │   确保不低于 minWeight (1)                                 │
  │                                                          │
  │ cooldown 后再次采样...                                      │
  └─────────────────────────────────────────────────────────┘
```

**观测到的典型重均衡过程**（来自实际日志）：

**Stream 1 — Products**（cooldown=3s, maxStep=2）：

| 时间 | 权重变化 | 方向 | deltaW | pressure_delta |
|------|---------|------|--------|---------------|
| 23:57:51 | `[1,1,8]` → `[3,1,6]` | `pool_slow` → `pool_fast` | 2 | 252.00 |
| 23:57:54 | `[3,1,6]` → `[5,1,4]` | `pool_slow` → `pool_fast` | 2 | 3.94 |
| 23:57:58 | `[5,1,4]` → `[7,1,2]` | `pool_slow` → `pool_fast` | 2 | 3.98 |
| 23:58:02 | `[7,1,2]` → `[8,1,1]` | `pool_slow` → `pool_fast` | 1 | 3.98 |

仅 **11 秒** 完成 4 次调整，权重从 `[1,1,8]` 翻转到 `[8,1,1]`，几乎完全镜像。

**Stream 2 — Customers**（cooldown=5s, maxStep=1）：

| 时间 | 权重变化 | 方向 | deltaW | pressure_delta |
|------|---------|------|--------|---------------|
| 23:57:51 | `[1,1,8]` → `[2,1,7]` | `pool_slow` → `pool_fast` | 1 | 252.00 |
| 23:57:57 | `[2,1,7]` → `[3,1,6]` | `pool_slow` → `pool_fast` | 1 | 3.98 |
| 23:58:03 | `[3,1,6]` → `[4,1,5]` | `pool_slow` → `pool_fast` | 1 | 3.94 |
| 23:58:09 | `[4,1,5]` → `[5,1,4]` | `pool_slow` → `pool_fast` | 1 | 3.29 |
| 23:58:15 | `[5,1,4]` → `[6,1,3]` | `pool_slow` → `pool_fast` | 1 | 3.52 |

Stream 2 由于 `maxStep=1` 和 `cooldown=5s`，调整更加保守平缓，约 **24 秒** 完成 5 次调整。稳定在 `[6,1,3]` 后 `pressure_delta` 降至 `< 3.0`，不再触发新的 rebalance。

---

## 项目结构

```
loadflow_demo_retail/
├── cmd/demo/
│   └── main.go                  # 主程序入口（~726 行）
├── config/
│   └── config.yaml              # 全量配置文件
├── internal/
│   ├── csvloader/
│   │   └── loader.go            # CSV 解析器（3 种数据拆分）
│   ├── handler/
│   │   ├── product.go           # Stream 1 Handler: 商品导入
│   │   ├── customer.go          # Stream 2 Handler: 客户交易聚合
│   │   └── order.go             # Stream 3 Handler: 订单状态机
│   ├── metrics/
│   │   └── prometheus.go        # 自定义 Prometheus 指标 + KeyTracker + WeightReporter
│   └── model/
│       └── models.go            # GORM 数据模型
├── deploy/
│   ├── docker-compose.yml       # Prometheus + Grafana 容器编排
│   ├── prometheus/
│   │   └── prometheus.yml       # Prometheus 采集配置
│   ├── grafana/
│   │   ├── provisioning/
│   │   │   ├── datasources/
│   │   │   │   └── prometheus.yml
│   │   │   └── dashboards/
│   │   │       └── dashboards.yml
│   │   └── dashboards/
│   │       └── loadflow.json    # 预配置仪表盘（9 个面板）
│   └── mysql/
│       └── init.sql             # 数据库初始化脚本
├── online_retail_II.csv         # Kaggle 数据集
├── go.mod
└── go.sum
```

---

## 快速开始

### 前置条件

- **Go** 1.23+
- **MySQL** 8.x（本地安装，非 Docker）
- **Docker & Docker Compose**（用于 Prometheus + Grafana）
- **Kaggle 数据集**：下载 [Online Retail II](https://www.kaggle.com/datasets/mashlyn/online-retail-ii-uci)，将 CSV 文件放置到项目根目录，命名为 `online_retail_II.csv`

### 1. 启动 MySQL

```bash
# 创建数据库
mysql -u root -p -e "CREATE DATABASE IF NOT EXISTS loadflow_demo CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# 或者用初始化脚本
mysql -u root -p < deploy/mysql/init.sql
```

> **注意**：请修改 `config/config.yaml` 中的 `database.dsn`，填入你的 MySQL 密码。

### 2. 启动 Prometheus + Grafana

```bash
cd deploy
docker compose up -d
```

验证容器正常运行：

```bash
docker ps
# 应看到 loadflow-prometheus (9090) 和 loadflow-grafana (3000)
```

### 3. 运行 Demo

```bash
cd /path/to/loadflow_demo_retail
go run ./cmd/demo/
```

Demo 运行约 3 分钟（`run_duration: 180s`），期间会打印实时日志：

- `[REBALANCE]` 行：每次权重调整的详情
- `[ProductHandler]`、`[CustomerHandler]`：Insert 进度
- `[Stream3]`：订单提交进度
- 最终输出 `DEMO RESULTS` 汇总

### 4. 查看 Grafana 仪表盘

打开浏览器访问：

```
http://localhost:3000/d/loadflow-retail-demo/loadflow-retail-demo
```

默认账号：`admin` / `admin`

> 仪表盘已通过 provisioning 自动加载，无需手动导入。

---

## 配置说明

核心配置位于 `config/config.yaml`，关键参数：

| 参数 | 值 | 说明 |
|------|------|------|
| `demo.max_rows` | 50000 | 限制 CSV 读取行数（0=全部） |
| `demo.run_duration` | 180s | 总运行时间 |
| `demo.handler_latency` | 15ms | 模拟 I/O 延迟，使 worker 数量成为瓶颈 |
| `demo.metrics_port` | 2112 | Prometheus 指标暴露端口 |
| `pools[].workers` | 8 / 4 / 2 | 三个 pool 的 worker 数量 |
| `routing[].initial_weights` | `[1, 1, 8]` | 初始权重（故意 80% 给 slow） |
| `scheduler.tick` | 1s | Scheduler 采样间隔 |
| `scheduler.policies[].cooldown` | 3s / 5s | 两次 rebalance 之间的冷却时间 |
| `scheduler.policies[].params.minPressureDelta` | 3.0 | 触发 rebalance 的最小压力差 |
| `scheduler.policies[].params.maxStep` | 2 / 1 | 每次调整的最大步长 |

**为什么需要 `handler_latency`？**

本地 MySQL `INSERT` 延迟 < 1ms，即使 `pool_slow`（2 workers）也能轻松处理 ~2000 msg/s，远超任何流的发射速率。加入 15ms 模拟延迟后：

| Pool | Workers | 理论吞吐 |
|------|---------|---------|
| `pool_fast` | 8 | ~533 msg/s |
| `pool_medium` | 4 | ~267 msg/s |
| `pool_slow` | 2 | ~133 msg/s |

此时 `pool_slow` 无法消化初始权重分配的 80% 流量（300×0.8 = 240 msg/s > 133），队列迅速堆积，`pressure_delta` 瞬间飙到 252，触发激进的 rebalance。

---

## 运行结果

以下是一次完整 Demo 运行的最终输出：

```
═══════════════════════════════════════════════════════════════
                    DEMO RESULTS
═══════════════════════════════════════════════════════════════
[Stream 1 - Products] Total inserted: 50000
[Stream 2 - Customers] Total inserted: 34726
[Stream 3 - Orders] Processed: 1909, Errors: 0
[StripedPool] Submitted=1909 Processed=1909 Panics=0
  Stripe-0: Submitted=241 Processed=241 QueueSize=0/256
  Stripe-1: Submitted=236 Processed=236 QueueSize=0/256
  Stripe-2: Submitted=232 Processed=232 QueueSize=0/256
  Stripe-3: Submitted=240 Processed=240 QueueSize=0/256
  Stripe-4: Submitted=242 Processed=242 QueueSize=0/256
  Stripe-5: Submitted=241 Processed=241 QueueSize=0/256
  Stripe-6: Submitted=237 Processed=237 QueueSize=0/256
  Stripe-7: Submitted=240 Processed=240 QueueSize=0/256

[Router] Final snapshot:
  stream_products -> pool_fast*8,pool_medium*1,pool_slow*1
  stream_customers -> pool_fast*6,pool_medium*1,pool_slow*3

[Bindings] Final weights:
  stream_products: pools=[pool_fast pool_medium pool_slow] weights=[8 1 1]
  stream_customers: pools=[pool_fast pool_medium pool_slow] weights=[6 1 3]

[DB Verification] Orders: total=1909, completed=1909
═══════════════════════════════════════════════════════════════
```

**结果要点**：

- **Stream 1**：全部 50,000 条商品记录成功写入 MySQL
- **Stream 2**：3 分钟内写入 34,726 笔客户交易（35,093 条源数据，部分因 context 超时未发送）
- **Stream 3**：1,909 笔订单全部完成状态机流转，**0 错误**，证明 StripedPool 保序完好
- **StripedPool** 各 Stripe 负载均匀（236~242），hash 分布良好
- **Products 权重**：从 `[1,1,8]` 收敛到 `[8,1,1]`（完全翻转）
- **Customers 权重**：从 `[1,1,8]` 收敛到 `[6,1,3]`（更温和的收敛）

---

## Grafana 仪表盘详解

Dashboard 包含 9 个面板，以下逐个解读并对照截图：

### Panel 1: Routing Weights — 路由权重变化

![Routing Weights](docs/images/routing_weights.png)

**PromQL**: `loadflow_router_weight`

**观测要点**：

这是整个 Demo 最核心的面板。图中展示了 6 条线（2 个 stream × 3 个 pool），清晰呈现 **阶梯式 rebalance** 过程：

- **黄色线**（stream_products → pool_fast）：从 **1** 阶梯上升到 **8**，每次跳 2 格（`maxStep=2`）
- **粉色线**（stream_products → pool_slow）：从 **8** 阶梯下降到 **1**，与黄线完全镜像
- **绿色线**（stream_customers → pool_fast）：从 **1** 缓慢爬升到 **6**，每次跳 1 格（`maxStep=1`）
- **红色线**（stream_customers → pool_slow）：从 **8** 缓步下降到 **3**
- **紫色/中间线**（两个 stream 的 pool_medium）：始终稳定在 **1**，因为 pool_medium 既不是最大压力池也不是最小压力池，不参与权重偷窃

**关键观察**：
1. 两条 stream 的收敛速度明显不同 — Products（cooldown=3s, maxStep=2）快速强势，Customers（cooldown=5s, maxStep=1）缓慢保守
2. 权重收敛后趋于稳定 — 因为 pressure_delta 降到 < 3.0 阈值以下
3. pool_medium 是"隐形人" — 权重不变意味着它的压力始终夹在 fast 和 slow 之间

---

### Panel 2: Queue Depth — 队列深度

![Queue Depth](docs/images/queue_depth.png)

**PromQL**: `loadflow_pool_pool_queue_depth`

**观测要点**：

- **蓝色线（pool_slow）**：开局瞬间飙到 **~500+**（Queue 容量 512，几乎打满！），因为 80% 流量涌入仅有 2 个 worker 的 pool_slow
- 随着 rebalance 生效（权重从 8 下降），pool_slow 的队列深度在 ~15 秒内急速下降，最终归零并在低位小幅波动
- **橙色线（pool_medium）** 和 **绿色线（pool_fast）**：始终贴近 0 — 它们有足够的 worker 处理分配到的流量
- 后半段 pool_slow 出现一些小幅脉冲（~20-30），这是来自 stream_customers 的残余流量（最终权重 `[6,1,3]`，pool_slow 仍分到 30%）

**这个面板直观展示了 rebalance 的效果**：从"一个 pool 快打满，其他 pool 空闲"到"负载趋于均衡"。

---

### Panel 3: Processing Rate — 处理速率

![Processing Rate](docs/images/processing_rate.png)

**PromQL**: `rate(loadflow_pool_pool_processed_total[30s])`

**观测要点**：

- **绿色线（pool_fast）**：稳态约 **350-380 ops/s**，是系统主力。8 个 worker × (1000/15) ≈ 533 ops/s 的理论上限，实际受路由权重和流量注入速率限制
- **蓝色线（pool_slow）**：稳态约 **70-90 ops/s**，与 2 worker × 66.7 ops/s 的理论值接近（处于满载状态）
- **橙色线（pool_medium）**：约 **50-60 ops/s**，较为稳定

三条线清晰反映了 **worker 数量差异 × 权重分配** 的叠加效应。pool_fast 获得最多权重且有最多 worker，吞吐绝对领先。

注意：在 Demo 结束时（右侧），所有线急速下降到 0 — 数据发送完毕，队列排空。

---

### Panel 4: Rebalance Events — 重均衡事件

![Rebalance Events](docs/images/rebalance_events.png)

**PromQL**: `increase(loadflow_rebalance_plan_total[30s])`

**观测要点**：

这是一个**柱状图**，展示 rebalance 的三种事件在时间轴上的分布：

- **蓝色（generated）**：Scheduler 产生了一个调整计划
- **橙色（applied）**：计划被成功应用，权重实际发生了变化
- **红色（rejected）**：计划被拒绝（例如 `maxPressure pool weight already at minimum 1`）

**图表解读**：

- 开局前 ~30s（23:58:00 ~ 23:58:30）：**密集的 generated + applied** — 这是压力最大、rebalance 最活跃的阶段
- 同时伴随大量 **rejected** — 当 pool_slow 的权重已经降到 `minWeight=1` 时，策略继续尝试偷权重但被拒绝
- 30s 后，事件柱几乎消失 — 系统达到稳态，`pressure_delta < 3.0`，不再触发新计划

这个面板帮助你理解 **rebalance 的密度和时机** — 初期激进、后期收敛。

---

### Panel 5: Sticky Violations — 粘性路由违规

![Sticky Violations](docs/images/sticky_violations.png)

**PromQL**: `increase(loadflow_routing_violations_total[30s])`

**观测要点**：

这是一个**橙色柱状图**，显示 Stream 2（stream_customers）在各时间窗口内的粘性违规数量。

- 粘性违规 = 某个 `CustomerID` 之前被路由到 pool_A，但由于 rebalance 改变了权重，此次被路由到了 pool_B
- **23:58:15 ~ 23:58:45**（rebalance 活跃期）：违规数最高，峰值达 **~7-8 次/30s**
- **23:59:00 之后**（权重稳定期）：违规仍持续存在但频率降低，约 **3-5 次/30s**
- **00:00:15**：出现一次高峰（~8.5 次），可能是 WRR 计数器周期性翻转导致的边界效应

**为什么稳态仍有违规？**

即使权重不再变化，WRR（加权轮询）本身的 cycle 内分配也不是完全确定性的。对相同 key 做 hash 后，其 slot 可能恰好落在 cycle 的不同位置。这是 WRR + Sticky 的已知 trade-off — 如果需要 100% 粘性保证，应使用 Consistent Hashing 路由器。

**业务影响**：违规次数较少（3 分钟内累计约 60 次），对于 34,726 条记录而言仅占 ~0.17%，对大多数业务场景来说是完全可接受的。

---

### Panel 6: Orders State Machine — 订单状态机

**PromQL**:

- `loadflow_orders_created_total`
- `loadflow_orders_completed_total`
- `loadflow_orders_failed_total`

**观测要点**：

这是一个 **Stat 面板**（大数字展示），显示三个计数器：

| 指标 | 值 | 含义 |
|------|------|------|
| **Created** | 1909 | 成功创建的订单数 |
| **Completed** | 1909 | 成功完成全流程的订单数 |
| **Failed** | 0 | 状态机流转失败的订单数 |

**Created = Completed = 1909**，**Failed = 0** — 这是 StripedPool **强有序保证** 的最有力证明。

如果把 Stream 3 换成不保序的普通 pool，`UPDATE ... WHERE status = 'processing'` 在 `INSERT` 之前执行时会匹配不到行，导致 `Failed > 0`。

---

### Panel 7: MySQL Write Latency — 数据库写入延迟

![MySQL Write Latency](docs/images/mysql_latency.png)

**PromQL**:

```promql
histogram_quantile(0.50, rate(loadflow_db_write_duration_seconds_bucket[30s]))  -- p50
histogram_quantile(0.95, rate(loadflow_db_write_duration_seconds_bucket[30s]))  -- p95
histogram_quantile(0.99, rate(loadflow_db_write_duration_seconds_bucket[30s]))  -- p99
```

**观测要点**：

图中多条线对应不同 `{stream, pool}` 组合的 p50、p95、p99 分位数。由于 handler 中包含 `time.Sleep(15ms)` 模拟延迟，实际延迟 = 15ms 模拟 + MySQL 真实写入时间。

- **p50（中位数）**：大部分集中在 **16-18ms** 左右（15ms 模拟 + ~1-3ms MySQL）
- **p95（第 95 百分位）**：约 **24-30ms**，偶尔跳到 35ms — 反映了 MySQL 连接池竞争和操作系统调度的抖动
- **p99（第 99 百分位）**：约 **25-38ms**，尾部延迟更高
- 在 Demo 结束阶段（00:01:00 ~ 00:01:15），出现一个 **p50 飙升到 ~60ms** 的尖刺 — 这是队列排空阶段剩余任务集中完成，叠加 MySQL 连接池回收造成的瞬时延迟升高

**p50/p95/p99 三层分位数** 是观测数据库性能的标准做法：
- **p50** 代表 "典型体验" — 大多数请求的延迟
- **p95** 代表 "慢请求" — 每 20 次请求中最慢的一次，通常是关注 SLA 的基准线
- **p99** 代表 "极端尾延迟" — 每 100 次请求中最慢的，反映系统在压力下的最差表现。p99 与 p50 的比值（本 Demo 中约 1.5-2x）越小说明系统延迟越稳定

---

### Panel 8: Stream Throughput — 流吞吐量

![Stream Throughput](docs/images/stream_throughput.png)

**PromQL**: `rate(loadflow_stream_messages_in_total[30s])`

**观测要点**：

- **蓝色线（stream_products）**：稳态约 **~300 ops/s**，与配置的 `rate: 300` 精确对应
- **绿色线（stream_customers）**：稳态约 **~200 ops/s**，与配置一致
- **黄色线（stream_orders）**：约 **30-60 ops/s**，在开头短暂冲高后迅速归零 — 因为只有 1,909 笔订单，不到 30 秒就全部提交完了

**注意 stream_products 的结束节拍**：蓝线在 00:00:42 附近开始下降（`[Stream1] All 50000 records sent`），而绿线在 00:00:50 截断（context 超时）。这说明 50,000 条产品在 ~2 分 52 秒内以恒定 300 msg/s 发送完毕（50000 / 300 ≈ 167s），与理论值吻合。

---

### Panel 9: Routing Distribution — 路由分发分布

![Routing Distribution](docs/images/routing_distribution.png)

**PromQL**: `rate(loadflow_router_routed_total[30s])`

**观测要点**：

这个面板展示 **每条 stream 的流量实际被分配到各 pool 的速率**，是 Panel 1（权重）在实际流量维度的直接投影。

- **黄色线（stream_products → pool_fast）**：稳态约 **230-240 ops/s** — 占 stream_products 总流量的 80%（权重 8/10）
- **绿色线（stream_customers → pool_fast）**：约 **110-130 ops/s** — 占 stream_customers 的 60%（权重 6/10）
- **红色线（stream_customers → pool_slow）**：约 **50-60 ops/s** — 权重 3/10
- **蓝色/紫色线（→ pool_medium）**：两条 stream 各约 **25-30 ops/s** — 权重 1/10

**这个面板与 Panel 1 对照来看最有价值**：Panel 1 显示"权重怎么变"，Panel 9 显示"流量实际怎么分"。你可以看到，当 rebalance 改变权重后，流量分布在下一秒就立即跟随变化，没有延迟。

---

## 关键设计决策

### 为什么绕过 Runtime 消息管线？

LoadFlow 的 `Runtime` 提供了完整的 Source → JSONCodec → Router → Pool → Handler 管线，但 Demo 中我们直接调用 `router.Route()` + `pool.Submit()` 并传入闭包，原因是：

1. **Handler 感知上下文**：闭包可以捕获 `poolName`，从而选择对应的 DB 连接。Runtime 管线中，handler 只收到 payload bytes，不知道自己在哪个 pool
2. **灵活的延迟注入**：`time.Sleep(handlerLatency)` 写在闭包里，精确控制每个任务的执行时间
3. **仍然使用 Runtime 的 PoolStat 采集**：通过 `rt.RegisterPool()` + `rt.UseRouter()` 注册后，Scheduler 的 `RuntimeMetricsProvider` 仍能正确抓取各 pool 的 `QueueDepth` 和 `ProcessRate`

### 为什么 pool_medium 的权重始终不变？

`pressure_rebalance` 策略每次只操作 **maxPressure** 和 **minPressure** 两端。pool_medium 的压力总是夹在 fast 和 slow 之间（既不是最大也不是最小），所以永远不参与权重偷窃。这是策略的预期行为 — 它倾向于一次修正最突出的失衡，而不会同时调整所有 pool。

### `minWeight=1` 的保护作用

配置中 `minWeight: 1.0` 确保每个 pool 至少保留权重 1，不会被完全踢出路由表。在日志中可以看到大量 `max pressure pool weight 1 already at minimum 1` 消息 — 这意味着策略试图继续降低 pool_slow 的权重到 0，但被 minWeight 规则阻止了。

---

## 技术栈

| 组件 | 版本/说明 |
|------|----------|
| **LoadFlow** | `github.com/DrWhoRC/loadflow` v0.1.1+ (main branch) |
| **Go** | 1.23+ |
| **MySQL** | 8.x（本地安装） |
| **GORM** | v1.31 |
| **Prometheus** | Docker (`prom/prometheus:latest`) |
| **Grafana** | Docker (`grafana/grafana:latest`) |
| **数据集** | Kaggle Online Retail II (~1.07M 行, CSV) |
| **Prometheus Client** | `prometheus/client_golang` v1.23 |
