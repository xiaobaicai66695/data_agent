# Audit Agent

Production-grade operations audit Agent for remote operation log collection, command auditing, offline resumption, and idempotent deduplication.

## Features

- **PTY Collection**: Host entire shell session via PTY, no trap/DEBUG hooks dependency
- **Offline Resumption**: SQLite local persistence, automatic retry after network recovery
- **Idempotent Deduplication**: Global unique MessageID + server-side deduplication table
- **Dynamic Batching**: Adaptive batch size adjustment based on server load and latency
- **Zero External Dependencies**: Pure Go + embedded SQLite, single binary distribution

## Tech Stack

- **Language**: Go 1.21+
- **Collection**: PTY pseudo-terminal (Linux)
- **Local Persistence**: SQLite (modernc.org/sqlite, pure Go, no CGO)
- **Reporting**: Async goroutine + dynamic batching
- **Network**: Offline cache, automatic resumption on recovery

## Architecture

```
┌───────────────────────────────────────────────────────────────┐
│                         Agent                                  │
│  ┌─────────┐    ┌───────────┐    ┌──────────┐    ┌─────────┐ │
│  │   PTY   │───▶│ Recorder  │───▶│ SQLite   │───▶│ Reporter│ │
│  │ Session │    │(Collection│    │ (Local)  │    │(Async)  │ │
│  └─────────┘    └───────────┘    └──────────┘    └────┬────┘ │
│                                                         │       │
│  ┌────────────┐                              ┌──────────▼──────────┐ │
│  │  Network   │─────────────────────────────▶│       Server         │ │
│  │  Monitor   │   (Health Check/Offline)      │    (Idempotent)     │ │
│  └────────────┘                              └──────────────────────┘ │
└───────────────────────────────────────────────────────────────────┘
```

### Core Modules

| Module | Path | Responsibility |
|--------|------|----------------|
| PTY Session | `internal/pty/` | Host shell session, collect input/output |
| Recorder | `internal/recorder/` | Record session data, generate audit logs |
| SQLite Store | `internal/storage/` | Local persistent storage |
| Reporter | `internal/reporter/` | Async upload, dynamic batching |
| Network Monitor | `internal/network/` | Health check, offline detection |

## Quick Start

### Build

```bash
# Build Agent
go build -o agent ./cmd/agent

# Build Server
go build -o server ./cmd/server
```

### Run

**Agent:**

```bash
./agent -server http://localhost:8080 \
        -db audit-agent.db \
        -count-threshold 20 \
        -time-threshold 5s
```

**Server:**

```bash
./server -listen :8080 \
        -pg-host localhost \
        -pg-port 5432 \
        -pg-user postgres \
        -pg-password postgres \
        -pg-database audit
```

## Configuration

### Agent Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SERVER_URL` | Server URL | - |
| `DB_PATH` | SQLite database path | `audit-agent.db` |
| `COUNT_THRESHOLD` | Count threshold | `20` |
| `TIME_THRESHOLD` | Time threshold | `5s` |

### Server Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `LISTEN_ADDR` | Listen address | `:8080` |
| `PG_HOST` | PostgreSQL host | `localhost` |
| `PG_PORT` | PostgreSQL port | `5432` |
| `PG_USER` | PostgreSQL user | `postgres` |
| `PG_PASSWORD` | PostgreSQL password | - |
| `PG_DATABASE` | PostgreSQL database | `audit` |

## API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | HEAD | Health check |
| `/api/v1/audit/upload` | POST | Upload audit records |
| `/api/v1/session` | GET | Query session info |
| `/api/v1/records` | GET | Query session records |

## Core Design

### State Machine

```
Pending -> Uploading -> Uploaded
    |                    |
    v                    v
  Failed              (success)
    |
    v
  (max retries exceeded)
```

### Idempotent Design

MessageID generation rule:

```
message_id = SHA256(session_id:seq_num:timestamp:counter:data_hash)
```

- session_id: Unique session identifier
- seq_num: Monotonically increasing sequence number
- timestamp: RFC3339Nano format
- counter: Global atomic counter
- data_hash: SHA256 hash of data content

### Dynamic Batching

| Condition | Action | Batch Change |
|-----------|--------|--------------|
| pending > 100 && latency < 500ms | INCREASE | ×1.5 |
| latency > 500ms | DECREASE | ×0.7 |

## License

MIT
[README.md](https://github.com/user-attachments/files/26426777/README.md)
