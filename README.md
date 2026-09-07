# elasticdata-inandout

Elasticsearch 索引数据导入/导出工具,用法类似 [elasticdump](https://github.com/elasticsearch-dump/elasticsearch-dump),仅支持 JSON 文件。

单文件 Go 实现,零第三方依赖。

## 特性

- **方向自动判断**:`--input`/`--output` 一个为 ES 地址、另一个为 JSON 文件,自动识别导出/导入
- **elasticdump 兼容格式**:两行式 JSON(元数据行 + `_source` 行),可与 elasticdump 互相导入导出
- **HTTPS 跳过证书校验**:https 一律不校验 SSL 证书
- **用户密码 URL 传入**:`https://user:pass@host:9200`
- **过滤导出**:通过 `--search-body` 传 ES 检索 DSL,只导出命中的文档
- **分批传输**:导出 scroll 分页、导入 `_bulk` 批量,`--limit` 控制每批条数
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
```

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--input` | 必填 | 输入:http(s) ES 地址(导出)或 JSON 文件路径(导入) |
| `--output` | 必填 | 输出:JSON 文件路径(导出)或 http(s) ES 地址(导入) |
| `--limit` | `1000` | 每批条数(scroll 分页 / bulk 批量) |
| `--search-body` | 空 | 导出过滤条件,ES 检索 DSL JSON,与 `_search` 请求体一致;省略 `size` 时自动注入 |

说明:

- ES 地址需包含索引名,如 `https://user:pass@host:9200/myindex`
- 导入时输出 URL 不含索引名,则使用文件元数据中的 `_index`
- `--search-body` 示例:`{"query":{"range":{"age":{"gte":30}}}}`

## 文件格式

与 elasticdump 一致,每两行为一个文档(第 1 行元数据、第 2 行 `_source`):

```json
{"_index":"myindex","_id":"1"}
{"name":"alice","age":30}
{"_index":"myindex","_id":"2"}
{"name":"bob","age":25}
```

## 测试

```bash
go test ./...
```
