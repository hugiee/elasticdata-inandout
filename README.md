# elasticdata-inandout

Elasticsearch 索引数据导入/导出工具,用法类似 [elasticdump](https://github.com/elasticsearch-dump/elasticsearch-dump),仅支持 JSON 文件。

单文件 Go 实现,零第三方依赖。

## 特性

- **方向自动判断**:`--input`/`--output` 一个为 ES 地址、另一个为 JSON 文件,自动识别导出/导入
- **elasticdump 兼容格式**:两行式 JSON(元数据行 + `_source` 行),可与 elasticdump 互相导入导出
- **HTTPS 跳过证书校验**:https 一律不校验 SSL 证书
- **用户密码 URL 传入**:`https://user:pass@host:9200`
- **过滤导出**:通过 `--search-body` 传 ES 检索 DSL,只导出命中的文档
- **多索引**:数据与 mapping/settings 导出均支持逗号分隔或通配符索引(如 `idx1,idx2`、`logs-*`)
- **mapping/settings 导出导入**:`--type=mapping,settings` 将索引结构与设置合并为一个 JSON 文件;导入时索引不存在则自动创建,已存在则只应用 ES 允许的增量
- **结构与数据一次搞定**:`--type=data,mapping,settings` 导出为单个混排文件(结构记录在前、数据在后),导入时先建/更新索引结构再灌数据
- **分批传输**:导出 scroll 分页、导入 `_bulk` 批量,`--limit` 控制每批条数
- **导出条数上限**:`--max-docs=N` 只导出前 N 条数据(只作用于数据记录,mapping/settings 结构记录始终完整导出)
- **进度条**:实时显示已处理/总数及百分比;导出总数经 `_count`(含相同过滤条件)获取,导入总数为文件行数/2,获取失败时降级为仅显示计数

## 安装

```bash
go build -o elasticdata-inandout .
```

## 使用

```bash
# 导出
elasticdata-inandout --input=https://user:pass@es:9200/myindex --output=myindex.json

# 导入
elasticdata-inandout --input=myindex.json --output=https://user:pass@es:9200/myindex

# 按条件导出(--search-body 为标准 ES _search 请求体 DSL)
elasticdata-inandout --input=https://user:pass@es:9200/myindex --output=out.json \
  --search-body='{"query":{"term":{"status":"active"}}}'

# 调整每批条数
elasticdata-inandout --input=https://user:pass@es:9200/myindex --output=out.json --limit=5000

# 多索引导出数据(每个文档行带各自 _index)
elasticdata-inandout --input='https://user:pass@es:9200/logs-*,metrics-*' --output=all.json

# 只导出前 100 条数据
elasticdata-inandout --input=https://user:pass@es:9200/myindex --output=first100.json --max-docs=100

# 导出 mapping 与 settings(合并一个文件)
elasticdata-inandout --input=https://user:pass@es:9200/myindex --output=myindex.ms.json --type=mapping,settings

# 导入 mapping 与 settings(目标索引不存在则创建,已存在则只应用 ES 允许的增量)
elasticdata-inandout --input=myindex.ms.json --output=https://user:pass@es:9200/newindex --type=mapping,settings

# 一次导出多个索引(逗号分隔或通配符;URL 含通配符时注意 shell 引号)
elasticdata-inandout --input='https://user:pass@es:9200/logs-*,metrics-*' --output=all.ms.json --type=mapping,settings

# 导入多索引文件(output URL 不含索引名,按文件中的索引逐个应用)
elasticdata-inandout --input=all.ms.json --output=https://user:pass@es:9200 --type=mapping,settings

# mapping + settings + data 一次导出(单个混排文件,结构记录在前)
elasticdata-inandout --input='https://user:pass@es:9200/i1,i2' --output=all.json --type=data,mapping,settings

# 导入混排文件:建/更新索引结构,再灌数据
elasticdata-inandout --input=all.json --output=https://user:pass@es:9200 --type=data,mapping,settings
```

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--input` | 必填 | 输入:http(s) ES 地址(导出)或 JSON 文件路径(导入) |
| `--output` | 必填 | 输出:JSON 文件路径(导出)或 http(s) ES 地址(导入) |
| `--limit` | `1000` | 每批条数(scroll 分页 / bulk 批量) |
| `--max-docs` | `0` | 最多导出的数据条数,`0` 表示不限;**多索引时为合计上限**(非每索引);只作用于数据记录,结构记录不受限;仅导出方向有效 |
| `--search-body` | 空 | 导出过滤条件,ES 检索 DSL JSON,与 `_search` 请求体一致;省略 `size` 时自动注入;仅 `--type=data` |
| `--type` | `data` | 导出/导入内容:`data`(文档)、`mapping`、`settings`,可逗号组合;仅 mapping/settings 时写单行 JSON(以索引名为键),含 data 时混排为两行式记录;mapping/settings 模式忽略 `--limit` |

说明:

- ES 地址需包含索引名,如 `https://user:pass@host:9200/myindex`
- 导入时输出 URL 不含索引名,则使用文件元数据中的 `_index`
- `--search-body` 示例:`{"query":{"range":{"age":{"gte":30}}}}`
- `--search-body` 中的 `size` 保持 ES 语义并覆盖 `--limit`:scroll 下它是**每批条数,不是总量上限**
- 只要前 N 条用 `--max-docs=N`:按 scroll 顺序取满即停,不再请求后续分页;要让“前”有确定含义,在 `--search-body` 里带 `sort`
- 不要用 `head` 截文件:混排或带结构记录的导入中截断会破坏记录结构
- mapping/settings 导入:目标索引不存在时按文件内容创建(`uuid`、`creation_date` 等私有设置自动剔除);已存在时只应用 ES 允许的增量 —— mapping 的 `properties` 仅新增字段(已有字段的定义差异跳过并警告),`_meta`、`dynamic` 等顶层参数原样保留;settings 中静态设置被拒时自动剔除后重试
- 结构文件里的索引名会作为 URL 路径使用,导入前按 ES 命名规则校验,含 `/`、`?` 或前导 `_` 等一律拒绝
- mapping/settings 支持一次多个索引:导出 URL 可用逗号分隔或通配符(如 `idx1,idx2`、`logs-*`);导入 URL 不含索引名时按文件中的索引逐个应用,指定索引名时仅支持单索引文件;任一索引失败即中断(已应用的索引不回退)
- 数据导出同样支持多索引 URL(逗号分隔或通配符):每个文档行携带其真实 `_index`,导入 URL 不含索引名时按元数据写回各自索引;`--search-body` 作为同一条件应用于全部匹配索引(DSL 字段需在各索引上类型兼容,否则 ES 会拒绝检索)
- 混排文件(`--type=data,mapping,settings`):导出时结构记录在前、数据在后;导入时先把同一索引的 mapping+settings **合并后一次性建索/更新**,再写数据。结构记录靠元数据行的 `_type`(`mapping`/`settings`)识别,普通文档的 `_type`(如 ES6 的 `doc`)不受影响
- 混排文件若含**多个源索引**的结构记录,导入时 output URL 不能带索引名(会明确报错,避免多个索引结构被静默合并);只含单个源索引时允许改名导入到 URL 指定的索引
- 导出写临时文件再原子替换目标文件:中途失败(凭据错误、索引不存在、DSL 非法等)不会销毁上一次的导出成果
- 混排文件 `--limit` 仍只用作 `_bulk` 与 scroll 批量;导入时结构应用失败会立即中止
- `--type` 仅含 mapping/settings 时,文件是单行 JSON(索引名作为键),不能用 `--type=data,...` 导入(会提示改用正确参数)
- 通配符会匹配隐藏/系统索引(如 `.security`),需要时用 ES 排除语法(如 `logs-*,-.security*`);URL 含通配符时请加 shell 引号

## 文件格式

与 elasticdump 一致,每两行为一个文档(第 1 行元数据、第 2 行 `_source`):

```json
{"_index":"myindex","_id":"1"}
{"name":"alice","age":30}
{"_index":"myindex","_id":"2"}
{"name":"bob","age":25}
```

mapping/settings 文件(`--type=mapping,settings`)为单行 JSON,以索引名为键(多索引导出即多个键):

```json
{"myindex":{"mappings":{"properties":{...}},"settings":{"index":{"number_of_shards":"1",...}}},"otherindex":{"mappings":{...},"settings":{...}}}
```

混排文件(`--type=data,mapping,settings`)仍是每记录 2 行,靠 `_type` 区分结构记录与文档,结构记录在前:

```json
{"_index":"i1","_type":"mapping"}
{"properties":{"a":{"type":"long"}}}
{"_index":"i1","_type":"settings"}
{"index":{"number_of_shards":"1"}}
{"_index":"i1","_id":"1"}
{"a":1}
```

## 测试

```bash
go test ./...
```
