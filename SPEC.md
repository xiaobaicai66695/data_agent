# 运维审计 Agent 技术方案

## 1. 项目概述

生产级运维审计 Agent，用于远程操作日志采集、命令审计、断网续传、幂等去重。

### 技术栈
- **开发语言**：Go
- **采集方式**：PTY 伪终端托管（ConPTY for Windows, pty for Linux）
- **本地持久化**：SQLite（modernc.org/sqlite，纯 Go 无 CGO 依赖）
- **上报策略**：异步后台协程 + 动态批处理
- **网络处理**：断网缓存、恢复自动续传
- **幂等机制**：全局唯一 message_id + 服务端去重表

## 2. 核心特性

### 2.1 状态机设计

每条 Record 有明确的状态流转：

```
Pending -> Uploading -> Uploaded
    |                    |
    v                    v
  Failed              (success)
    |
    v
  (max retries exceeded)
```

| 状态 | 值 | 说明 |
|------|-----|------|
| StatusPending | 0 | 等待上传 |
| StatusUploading | 1 | 上传中 |
| StatusUploaded | 2 | 上传成功 |
| StatusFailed | 3 | 上传失败（超过最大重试次数） |

### 2.2 幂等设计

**message_id 生成规则**：
```go
message_id = SHA256(session_id:seq_num:timestamp:counter:data_hash)
```

- session_id: 唯一会话标识（基于 hostname+username+timestamp 生成）
- seq_num: 会话内单调递增序号
- timestamp: RFC3339Nano 格式
- counter: 全局原子计数器
- data_hash: 数据内容的 SHA256 哈希

**服务端去重**：
- 服务端维护 `idempotency` 表
- message_id 作为唯一键
- 重复请求返回成功但不重复入库

### 2.3 断网续传逻辑

**检测机制**：
- Network Monitor 每 3 秒检测 /health 端点
- 连续 3 次失败判定为离线
- 状态变化通过 channel 通知 Reporter

**续传流程**：
```
Server Online -> Server Offline
     |              |
     v              v
  正常上传      暂停上传
     |              |
     v              v
Server Recovery  <- 等待恢复
     |
     v
  自动触发 flushPendingRecords()
  从 SQLite 读取所有 Pending/Failed 记录
  按序上传
```

### 2.4 双阈值动态批处理

**阈值参数**：
- `countThreshold`: 数量阈值，默认 20 条
- `timeThreshold`: 时间阈值，默认 5 秒

**触发条件**（满足任一即触发）：
1. 缓冲区记录数 >= countThreshold
2. 距上次 flush 时间 >= timeThreshold

**批处理流程**：
```
SubmitRecord()
    |
    v
添加到 batchBuffer
    |
    v
是否达到 countThreshold?
    |
    Yes -> triggerFlush() -> doFlush() -> uploadBatch()
    |
    No
    |
    v
batchWorker() 每秒检查 timeThreshold
    |
    v
满足则 triggerFlush()
```

### 2.5 动态批次大小调整

**DynamicBatcher** 根据服务器负载和延迟动态调整批次大小：

**调整策略**：
| 条件 | 操作 | 批次大小变化 |
|------|------|-------------|
| pending > 100 && latency < 500ms | INCREASE | currentSize * 1.5 |
| latency > 500ms | DECREASE | currentSize * 0.7 |
| 其他 | 保持 | 不变 |

**参数**：
- `baseSize`: 20（初始批次大小）
- `minSize`: 5（最小批次大小）
- `maxSize`: 100（最大批次大小）
- `backlogThreshold`: 100（积压阈值）
- `latencyThreshold`: 500ms（延迟阈值）
- `adjustInterval`: 10s（调整间隔）

**调整算法**：
```
每 10 秒检查一次：
if pendingCount > backlogThreshold AND serverLatency < latencyThreshold:
    currentSize = min(currentSize * increaseFactor, maxSize)
    log "INCREASE batch: X -> Y"

else if serverLatency > latencyThreshold:
    currentSize = max(currentSize * decreaseFactor, minSize)
    log "DECREASE batch: X -> Y"
```

**日志示例**：
```
[DynamicBatch] Backlog high(150>100), latency low(200ms), INCREASE batch: 20 -> 30
[DynamicBatch] Latency high(800ms>500ms), DECREASE batch: 30 -> 21
[Reporter] Successfully uploaded 25 records (latency: 150ms, currentBatchSize: 30)
```

## 3. 模块架构

### 3.1 模块划分

```
audit-agent/
├── cmd/
│   ├── agent/main.go       # Agent 主程序
│   └── server/main.go      # Server 主程序
├── internal/
│   ├── network/
│   │   └── monitor.go      # 网络状态监控
│   ├── pty/
│   │   └── session.go      # PTY 会话管理
│   ├── recorder/
│   │   └── recorder.go     # 录音/记录模块
│   ├── reporter/
│   │   └── reporter.go     # 异步上报模块
│   └── storage/
│       └── sqlite.go       # SQLite 持久化
└── pkg/
    ├── audit/
    │   └── types.go        # 类型定义
    └── idgen/
        └── idgen.go        # ID 生成器
```

### 3.2 数据库表结构

**records 表（Agent 使用 SQLite，Server 使用 PostgreSQL）**：
```sql
CREATE TABLE records (
    id TEXT PRIMARY KEY,
    message_id TEXT UNIQUE NOT NULL,
    session_id TEXT NOT NULL,
    seq_num BIGINT NOT NULL,
    timestamp TIMESTAMPTZ NOT NULL,
    type INTEGER NOT NULL,
    data TEXT NOT NULL,
    status INTEGER NOT NULL DEFAULT 0,
    retry_cnt INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL
);
```

**sessions 表**：
```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    pid INTEGER,
    shell TEXT NOT NULL,
    start_time TIMESTAMPTZ NOT NULL,
    end_time TIMESTAMPTZ,
    rows INTEGER NOT NULL DEFAULT 40,
    cols INTEGER NOT NULL DEFAULT 120,
    hostname TEXT,
    username TEXT
);
```

**idempotency 表（Server）**：
```sql
CREATE TABLE idempotency (
    message_id TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL
);
```

### 3.3 架构说明

| 组件 | 数据库 | 说明 |
|------|--------|------|
| Agent | SQLite (modernc.org/sqlite) | 本地高可用缓存，断网续传 |
| Server | PostgreSQL | 持久化存储，高可用存储 |

## 4. 使用方式

### 4.1 Agent

```bash
./agent.exe -server http://localhost:8080 \
             -db audit-agent.db \
             -count-threshold 20 \
             -time-threshold 5s
```

### 4.2 Server

```bash
./server.exe -listen :8080 \
             -pg-host localhost \
             -pg-port 5432 \
             -pg-user postgres \
             -pg-password postgres \
             -pg-database audit
```

### 4.3 API 接口

| 端点 | 方法 | 说明 |
|------|------|------|
| /health | HEAD | 健康检查 |
| /api/v1/audit/upload | POST | 上传审计记录 |
| /api/v1/session | GET | 查询会话信息 |
| /api/v1/records | GET | 查询会话内的记录 |

## 5. 关键设计决策

### 5.1 为什么使用纯 Go SQLite

- 支持 `CGO_ENABLED=0` 编译
- 单二进制分发
- 无外部依赖

### 5.2 为什么不使用单条记录上传

- 减少 HTTP 请求次数
- 提高批量处理效率
- 支持事务性入库

### 5.3 如何保证顺序

- seq_num 单调递增
- 服务端按 seq_num ASC 排序
- Batch 内记录保持顺序
