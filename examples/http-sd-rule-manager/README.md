# http-sd-rule-manager

基于 HTTP 的 Prometheus `http_sd` 与 `http_rule_files` 统一管理服务。提供 RESTful 增删改查 API，持久化到 JSON 文件，Prometheus 通过 `http_sd_config` 和 `http_rule_files` 定期拉取，内容变更时自动热更新，无需重启或手动调用 `/-/reload`。

## 功能简介

- **SD 文件管理**：管理服务发现 target groups，对应 Prometheus `http_sd_config`
- **Rule 文件管理**：管理告警/录制规则组，对应 Prometheus `http_rule_files`
- **增删改查 API**：每个文件支持 List / Create / Read / Update / Delete
- **文件持久化**：所有改动原子写入 `data/sd/` 与 `data/rules/` 目录下的 JSON 文件
- **首次启动自动 seed**：预置 `default` SD 文件与 `default` 规则文件，开箱即用
- **热更新**：任何 CRUD 操作在下个 `refresh_interval` 后对 Prometheus 生效（SHA256 变更检测）

## 目录结构

```
examples/http-sd-rule-manager/
├── main.go           # 主程序（Gin + 文件存储）
├── go.mod            # 独立 module
├── prometheus.yml    # 示例 Prometheus 配置
└── data/             # 运行时生成
    ├── sd/
    │   └── default.json
    └── rules/
        └── default.json
```

## 启动

```bash
cd examples/http-sd-rule-manager
go run .
```

服务监听 `:9092`。Windows 下如遇 `go.work` 冲突，设置 `$env:GOWORK="off"`。

## API 总览

### 管理接口

| 资源 | 方法 | 路径 | 说明 |
|---|---|---|---|
| SD | GET | `/api/v1/sd` | 列出所有 SD 文件名 |
| SD | GET | `/api/v1/sd/{name}` | 读取单个 SD 文件（含 groups 信封） |
| SD | POST | `/api/v1/sd/{name}` | 创建 SD 文件（body 为 target groups 数组） |
| SD | PUT | `/api/v1/sd/{name}` | 更新 SD 文件（整体替换） |
| SD | DELETE | `/api/v1/sd/{name}` | 删除 SD 文件 |
| Rules | GET | `/api/v1/rules` | 列出所有 Rule 文件名 |
| Rules | GET | `/api/v1/rules/{name}` | 读取单个 Rule 文件 |
| Rules | POST | `/api/v1/rules/{name}` | 创建 Rule 文件 |
| Rules | PUT | `/api/v1/rules/{name}` | 更新 Rule 文件 |
| Rules | DELETE | `/api/v1/rules/{name}` | 删除 Rule 文件 |

### Prometheus 拉取端点（返回纯 JSON 数组，无信封）

| 端点 | 用途 | 对应配置 |
|---|---|---|
| `GET /sd/{name}` | http_sd target groups | `scrape_configs[].http_sd_configs[].url` |
| `GET /rules/{name}` | http_rule_files 规则组 | `http_rule_files[].url` |

## 数据格式

### SD target group（http_sd 期望的 JSON 数组）

```json
[
  {
    "labels": {"job": "web", "env": "prod"},
    "targets": ["10.0.0.1:9090", "10.0.0.2:9090"]
  },
  {
    "labels": {"job": "web", "env": "staging"},
    "targets": ["10.0.0.3:9090"]
  }
]
```

### Rule group（http_rule_files 期望的 JSON 数组）

```json
[
  {
    "name": "service-health",
    "interval": "30s",
    "rules": [
      {
        "alert": "ServiceDown",
        "expr": "up == 0",
        "for": "5m",
        "labels": {"severity": "critical"},
        "annotations": {
          "summary": "Service {{ $labels.instance }} is down",
          "description": "{{ $labels.instance }} of job {{ $labels.job }} has been down for more than 5 minutes."
        }
      }
    ]
  },
  {
    "name": "recordings",
    "interval": "1m",
    "rules": [
      {"record": "job:up_ratio", "expr": "avg by (job) (up)"}
    ]
  }
]
```

字段名与 Prometheus YAML 规则文件一致（`name`/`interval`/`rules`/`alert`/`record`/`expr`/`for`/`labels`/`annotations`）。

## 使用示例

### 1. 列出 / 读取

```bash
# 列出所有 SD 文件
curl http://localhost:9092/api/v1/sd
# {"files":["default"]}

# 读取 default SD 文件（带信封）
curl http://localhost:9092/api/v1/sd/default

# Prometheus 实际拉取的端点（纯数组）
curl http://localhost:9092/sd/default
```

### 2. 创建

```bash
# 创建 SD 文件 web
curl -X POST http://localhost:9092/api/v1/sd/web \
  -H 'Content-Type: application/json' \
  -d '[{"labels":{"job":"web","env":"prod"},"targets":["10.0.0.1:9090","10.0.0.2:9090"]}]'

# 创建 Rule 文件 alerts
curl -X POST http://localhost:9092/api/v1/rules/alerts \
  -H 'Content-Type: application/json' \
  -d '[{"name":"service-health","interval":"30s","rules":[{"alert":"ServiceDown","expr":"up == 0","for":"5m","labels":{"severity":"critical"}}]}]'
```

### 3. 更新（整体替换）

```bash
# 更新 alerts：将 for 从 5m 改为 1m，新增 recording 规则组
curl -X PUT http://localhost:9092/api/v1/rules/alerts \
  -H 'Content-Type: application/json' \
  -d '[{"name":"service-health","interval":"30s","rules":[{"alert":"ServiceDown","expr":"up == 0","for":"1m","labels":{"severity":"warning"}}]},{"name":"recordings","interval":"1m","rules":[{"record":"job:up_ratio","expr":"avg by (job) (up)"}]}]'
```

### 4. 删除

```bash
curl -X DELETE http://localhost:9092/api/v1/sd/web
curl -X DELETE http://localhost:9092/api/v1/rules/alerts
```

## Prometheus 配置

`prometheus.yml` 已同时配置 `http_sd_configs` 和 `http_rule_files`：

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

http_rule_files:
  - url: http://localhost:9092/rules/default
    refresh_interval: 15s

scrape_configs:
  - job_name: http_sd_managed
    http_sd_configs:
      - url: http://localhost:9092/sd/default
        refresh_interval: 15s

  - job_name: prometheus
    static_configs:
      - targets: [localhost:9090]
```

启动：

```bash
./prometheus \
  --config.file=examples/http-sd-rule-manager/prometheus.yml \
  --storage.tsdb.path=data/
```

## 热更新验证

1. 修改管理服务中的 SD 或 Rule 文件（POST/PUT/DELETE）
2. 等待最多 `refresh_interval`（默认 15s）
3. Prometheus 日志出现 `HTTP rule change detected, reloading rules`
4. 查询 `/api/v1/rules` 或 `/api/v1/targets` 确认变更已生效

无需重启 Prometheus，也无需调用 `/-/reload`。

## 实现细节

- **并发安全**：`store` 使用 `sync.RWMutex` 保护所有读写
- **原子写入**：先写 `.tmp` 文件再 `os.Rename`，避免部分写入
- **路径校验**：拒绝包含 `/`、`\`、`..` 的文件名，防止目录穿越
- **返回格式**：管理 API 返回带 `name`/`groups`/`message` 的信封对象；Prometheus 拉取端点返回纯 JSON 数组，符合 `http_sd` 和 `http_rule_files` 的 schema 要求

## 许可证

Apache License 2.0，与 Prometheus 主项目一致。
