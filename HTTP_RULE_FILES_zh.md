# HTTP Rule Files（HTTP 动态规则加载）

## 功能简介

除了传统的 `rule_files` 字段外，Prometheus 还支持通过 `http_rule_files` 字段从 HTTP 端点加载告警规则和记录规则。该机制与 `http_sd` 服务发现类似：Prometheus 按固定周期轮询每个配置的 HTTP 端点，当响应内容发生变化时**自动热更新**规则，无需重启，也无需调用 `/-/reload`。原有基于文件的 `rule_files` 行为完全保留，两个字段可在同一份配置中并存使用。

### 核心特性

- 新增 `http_rule_files` 配置字段，与 `rule_files` 平行存在
- 每个端点支持完整的 `HTTPClientConfig`（TLS、`basic_auth`、`bearer_token`、`authorization`、`oauth2`、`proxy_url`、`headers`）
- 基于 SHA256 哈希的内容变更检测
- JSON 响应格式：规则组数组，字段名与 YAML 规则文件保持一致
- 拉取失败时保留上次成功加载的规则
- 默认 60s 刷新间隔（可通过 `refresh_interval` 配置）
- 自动热更新，无需重启或 `/-/reload`

## 配置方式

`http_rule_files` 中的每一项都接受完整的 `HTTPClientConfig`（包括 TLS、`basic_auth`、`bearer_token`、`authorization`、`oauth2`、`proxy_url`、`headers` 等），与 `http_sd_configs` 完全一致，此外还需要一个 `url` 和一个 `refresh_interval`（默认 `60s`）。

```yaml
# 传统的基于文件的规则可与 http_rule_files 并存
rule_files:
  - "rules/*.yml"

# 基于 HTTP 的规则文件。每个端点按 refresh_interval 周期轮询，
# 当响应内容变化时（通过 SHA256 对比）自动热更新
http_rule_files:
  - url: http://example.com/rules.json
    refresh_interval: 30s
  - url: https://secret.example.com/rules
    refresh_interval: 60s
    basic_auth:
      username: alice
      password: secret
    tls_config:
      ca_file: /etc/ssl/certs/ca.pem
```

### 字段说明

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `url` | string | （必填） | HTTP/HTTPS 端点地址，必须以 `http://` 或 `https://` 开头 |
| `refresh_interval` | duration | `60s` | 轮询间隔 |
| `basic_auth` | object | - | 基本认证（username/password） |
| `bearer_token` | string | - | Bearer Token（与 `bearer_token_file` 互斥） |
| `bearer_token_file` | string | - | Bearer Token 文件路径 |
| `authorization` | object | - | Authorization 头（credentials/credentials_file） |
| `oauth2` | object | - | OAuth2 配置 |
| `tls_config` | object | - | TLS 配置（ca_file/cert_file/key_file/server_name 等） |
| `proxy_url` | string | - | 代理地址 |
| `headers` | map | - | 自定义请求头（不能包含 `host`、`content-type`、`user-agent` 等保留头） |

## 响应格式

HTTP 端点必须返回 `Content-Type: application/json`，响应体为一个**规则组 JSON 数组**。字段名与 YAML 规则文件格式保持一致（`name`、`interval`、`rules`、`record`、`alert`、`expr`、`for`、`labels`、`annotations`）：

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
        "labels": { "severity": "critical" },
        "annotations": {
          "summary": "Service {{ $labels.instance }} is down",
          "description": "{{ $labels.instance }} of job {{ $labels.job }} has been down for more than 5 minutes."
        }
      },
      {
        "record": "job:up_ratio",
        "expr": "avg by (job) (up)",
        "labels": { "team": "sre" }
      }
    ]
  }
]
```

### 字段说明

#### 规则组（RuleGroup）

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 是 | 规则组名称，同一 Prometheus 实例内必须唯一 |
| `interval` | duration | 否 | 规则评估间隔，未设置时使用全局 `evaluation_interval` |
| `rules` | array | 是 | 规则列表，可包含告警规则和记录规则 |

#### 告警规则（AlertingRule）

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `alert` | string | 是 | 告警名称 |
| `expr` | string | 是 | PromQL 表达式 |
| `for` | duration | 否 | 持续多久触发告警 |
| `labels` | map | 否 | 附加标签 |
| `annotations` | map | 否 | 注释信息 |

#### 记录规则（RecordingRule）

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `record` | string | 是 | 记录规则名称 |
| `expr` | string | 是 | PromQL 表达式 |
| `labels` | map | 否 | 附加标签 |

## 热更新行为

- 每个端点按其 `refresh_interval` 周期轮询。响应体会被计算 SHA256 哈希并与上一次的哈希比较，只有哈希变化时才会重新应用规则。
- 检测到内容变化后，Prometheus 会基于当前已应用的配置重新执行一次规则 reload，无需完整 config reload、无需重启、无需 `/-/reload`。
- 当拉取失败时（HTTP 错误、Content-Type 非 JSON、JSON 解析失败或规则校验失败），Prometheus 会**保留该端点上一次成功加载的规则**并打印告警日志。单个端点故障不会影响其他端点的规则 reload。
- 端点的首次 `Load` 是同步执行的，确保规则管理器初始化时就能看到当前规则；后续更新由后台轮询协程触发。

## 使用说明

- `http_rule_files` 在 **Agent 模式下不允许使用**（与 `rule_files` 限制一致）。
- 同一配置中 `http_rule_files` 不允许出现重复的 URL，配置加载时会校验并报错。
- `HTTPClientConfig` 中的相对路径字段（如 `bearer_token_file`、`ca_file`）会相对于主配置文件所在目录解析，与其他配置段保持一致。
- 完整的可运行示例（基于 Gin 编写的 HTTP 规则服务 + 配套的 `prometheus.yml`）位于 [`examples/http-rule-files/`](./examples/http-rule-files)。

## 快速验证

### 1. 启动测试用的 HTTP 规则服务

测试服务监听 9091 端口，提供规则拉取、变体切换和状态查询接口：

```bash
cd examples/http-rule-files
GOWORK=off go run .
```

服务启动后可用的接口：

| 端点 | 说明 |
| --- | --- |
| `GET /rules` | 返回 JSON 规则数组，供 Prometheus `http_rule_files` 拉取 |
| `GET /toggle` | 切换规则变体（0↔1），触发 SHA256 变化，测试热更新 |
| `GET /status` | 查看当前变体和规则内容 |

测试服务内置两种规则变体：

- **variant 0**：`up == 0`，`for: 5m`，severity=critical
- **variant 1**：`up == 0`，`for: 1m`，severity=warning + 额外 recording rule `job:up_ratio`

### 2. 启动 Prometheus

使用新编译的 Prometheus 二进制启动：

```bash
./prometheus --config.file=examples/http-rule-files/prometheus.yml \
             --storage.tsdb.path=data/
```

### 3. 验证规则已加载

```bash
curl http://localhost:9090/api/v1/rules
```

应能看到名为 `service-health` 的规则组，其中包含 `ServiceDown` 告警规则。

### 4. 触发规则热更新

```bash
curl http://localhost:9091/toggle
```

返回示例：

```json
{
  "current_variant": 1,
  "previous_variant": 0,
  "message": "rules changed; Prometheus should hot-reload within the next refresh interval"
}
```

### 5. 等待自动应用

在一个 `refresh_interval` 周期内（示例配置为 15s），Prometheus 会自动重新拉取并应用新规则。可再次查询 `/api/v1/rules` 或查看 Prometheus 日志中的 `HTTP rule change detected, reloading rules` 信息确认热更新成功。

## 编译说明

### 标准编译（不含前端资源）

```bash
go build -o prometheus ./cmd/prometheus/
```

### 编译含前端资源的二进制（推荐）

```bash
# 1. 构建前端资源（需要 Node.js 22+ 和 pnpm 11+）
cd web/ui
pnpm install
pnpm --filter "@prometheus-io/lezer-promql" run build
pnpm --filter "@prometheus-io/codemirror-promql" run build
cd react-app && pnpm install && pnpm run build && cd ..
mv react-app/build static/react-app
pnpm --filter @prometheus-io/mantine-ui run build
mv mantine-ui/dist static/mantine-ui
cd ../..

# 2. 生成 embed.go（gzip 压缩 + go:embed 指令）
# Linux/macOS:
scripts/compress_assets.sh
# Windows (PowerShell):
powershell -ExecutionPolicy Bypass -File scripts/compress_assets.ps1

# 3. 用 builtinassets tag 编译
go build -tags builtinassets -o prometheus ./cmd/prometheus/
```

### 交叉编译 Linux amd64

```bash
GOOS=linux GOARCH=amd64 go build -tags builtinassets -o prometheus-linux-amd64 ./cmd/prometheus/
```

## 配置校验

使用 `promtool` 校验配置：

```bash
./promtool check config examples/http-rule-files/prometheus.yml
```

常见错误：

| 错误信息 | 原因 |
| --- | --- |
| `URL scheme must be 'http' or 'https'` | URL 协议必须是 http 或 https |
| `URL is missing` | 未配置 `url` 字段 |
| `found duplicate http_rule_files URL` | 同一配置中存在重复的 URL |
| `field http_rule_files is not allowed in agent mode` | Agent 模式下不允许使用 |

## 实现细节

### 代码结构

| 文件 | 说明 |
| --- | --- |
| `rules/httprules/config.go` | `HTTPRuleFileConfig` 配置类型及 YAML 解析校验 |
| `rules/httprules/provider.go` | `Provider` 周期轮询、SHA256 变更检测、JSON 解析、失败保留逻辑 |
| `model/rulefmt/rulefmt.go` | 新增 `ValidateGroups` 方法用于非 YAML 来源的规则校验 |
| `config/config.go` | 新增 `HTTPRuleFiles` 字段、`SetDirectory`、Agent 模式检查、URL 去重 |
| `rules/manager.go` | 新增 `HTTPGroupLoader` 通过 URL 前缀分发到 `FileLoader` 或 `HTTPRuleProvider` |
| `cmd/prometheus/main.go` | 集成 `Provider` 到 `run.Group`，`onUpdate` 回调触发仅规则的 reload |

### 工作流程

1. Prometheus 启动时读取 `http_rule_files` 配置，为每个端点创建独立的 HTTP client
2. `Provider.Run` 为每个端点启动一个 goroutine，按 `refresh_interval` 周期拉取规则
3. 拉取响应后计算 SHA256 哈希，与上次比较：
   - 哈希相同：跳过，等待下一个周期
   - 哈希不同：JSON 解析 → `ValidateGroups` 校验 → 更新缓存 → 调用 `onUpdate` 回调
4. `onUpdate` 回调触发 `rulesReloader(appliedCfg)`，重新调用 `ruleManager.Update`
5. `HTTPGroupLoader.Load` 通过 URL 前缀（`http://`/`https://`）分发：
   - HTTP URL：从 `Provider` 缓存读取
   - 文件路径：透传给 `FileLoader` 处理

### 失败处理策略

- **首次拉取失败**：返回空的 `RuleGroups`（不报错），确保单个端点故障不影响整体 reload
- **后续拉取失败**：保留上次成功加载的规则，打印告日志
- **JSON 解析失败**：同上，保留旧规则
- **规则校验失败**：同上，保留旧规则
- **Content-Type 非 JSON**：拒绝响应，保留旧规则

## 许可证

Apache License 2.0，详见 [LICENSE](./LICENSE)。
