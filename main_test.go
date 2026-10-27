package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// 假 ES:内存 map 按 _id 存文档,支持 scroll search 与 _bulk;另支持 _mapping/_settings 与建索
type fakeES struct {
	docs      map[string]string
	docIndex  map[string]string // 文档所在索引(缺省 "idx")
	lastPath  string            // 最近一次 _search 请求路径
	lastBulk  string            // 最近一次 _bulk 请求体
	lastQuery url.Values        // 最近一次 _search 的查询参数
	lastBody  string            // 最近一次 _search 的请求体

	mapping  string            // GET _mapping 响应
	settings string            // GET _settings 响应
	exists   map[string]bool   // 已存在的索引
	created  map[string]string // 建索请求: 索引名 -> 请求体
	updated  []string          // _mapping/_settings 更新记录("路径 请求体")

	failStaticOnce bool // 置位后首次 PUT _settings 返回静态设置错误(一次)
}

func (f *fakeES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/_count"):
		w.Write([]byte(`{"count":2}`))
	case strings.HasSuffix(r.URL.Path, "/_search") && r.URL.Query().Get("scroll") != "":
		f.lastPath = r.URL.Path
		f.lastQuery = r.URL.Query()
		b, _ := io.ReadAll(r.Body)
		f.lastBody = string(b)
		w.Write([]byte(`{"_scroll_id":"s1","hits":{"hits":[`))
		ids := make([]string, 0, len(f.docs))
		for id := range f.docs {
			ids = append(ids, id)
		}
		for i, id := range ids {
			if i > 0 {
				w.Write([]byte(","))
			}
			idx := f.docIndex[id]
			if idx == "" {
				idx = "idx"
			}
			fmt.Fprintf(w, `{"_index":%q,"_id":%q,"_source":%s}`, idx, id, f.docs[id])
		}
		w.Write([]byte(`]}}`))
	case r.URL.Path == "/_search/scroll":
		w.Write([]byte(`{"_scroll_id":"","hits":{"hits":[]}}`))
	case r.URL.Path == "/_bulk":
		body, _ := io.ReadAll(r.Body)
		f.lastBulk = string(body)
		for _, line := range strings.Split(string(body), "\n") {
			if line == "" {
				continue
			}
			var act map[string]map[string]any
			if json.Unmarshal([]byte(line), &act) != nil || act["index"] == nil {
				continue // 非行动行(如 _source 行)
			}
			id, _ := act["index"]["_id"].(string)
			idx, _ := act["index"]["_index"].(string)
			f.docs[id] = idx
		}
		w.Write([]byte(`{"errors":false}`))
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/_mapping"):
		w.Write([]byte(f.mapping))
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/_settings"):
		w.Write([]byte(f.settings))
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/_mapping"):
		b, _ := io.ReadAll(r.Body)
		f.updated = append(f.updated, r.URL.Path+" "+string(b))
		w.Write([]byte(`{"acknowledged":true}`))
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/_settings"):
		b, _ := io.ReadAll(r.Body)
		f.updated = append(f.updated, r.URL.Path+" "+string(b))
		if f.failStaticOnce {
			f.failStaticOnce = false
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"root_cause":[{"type":"illegal_argument_exception","reason":"can not update non dynamic settings [[index.number_of_shards]] for open indices [idx]"}]},"status":400}`))
			return
		}
		w.Write([]byte(`{"acknowledged":true}`))
	case r.Method == "PUT": // 建索引: PUT /{索引名}
		name := strings.Trim(r.URL.Path, "/")
		if f.exists[name] {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"resource_already_exists_exception","reason":"exists"}}`))
			return
		}
		if f.exists == nil {
			f.exists = map[string]bool{}
		}
		f.exists[name] = true
		b, _ := io.ReadAll(r.Body)
		if f.created == nil {
			f.created = map[string]string{}
		}
		f.created[name] = string(b)
		w.Write([]byte(`{"acknowledged":true}`))
	default:
		w.Write([]byte("{}"))
	}
}

func TestEndToEnd(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{docs: map[string]string{
		"1": `{"a":1}`,
		"2": `{"b":"x y"}`,
	}})
	defer srv.Close()

	dir := t.TempDir()
	file := filepath.Join(dir, "out.json")

	// 导出
	if err := export(srv.URL, "idx", file, exportOpts{limit: 100}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("期望 4 行(2 文档 x 2),得到 %d", len(lines))
	}

	// 导入到新的假 ES
	dst := &fakeES{docs: map[string]string{}}
	srv2 := httptest.NewTLSServer(dst)
	defer srv2.Close()
	// export 用的是 srv 的 client(跳过证书校验),importFile 同样走全局 client
	if err := importFile(file, srv2.URL, "", 1); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(dst.docs) != 2 {
		t.Fatalf("期望导入 2 条,得到 %d", len(dst.docs))
	}
	for _, id := range []string{"1", "2"} {
		if dst.docs[id] == "" {
			t.Errorf("文档 %s 未导入", id)
		}
	}
	// 重复 _id 应覆盖而非报错: _bulk 必须用 index 动作(而非 create)
	if !strings.Contains(dst.lastBulk, `"index"`) {
		t.Errorf("_bulk 应使用 index 动作以覆盖同 _id 文档: %s", dst.lastBulk)
	}
}

func TestProgress(t *testing.T) {
	pb := &progress{w: io.Discard, label: "导出", total: 100}
	pb.show(50)
	// show 只写 stderr,无返回值;这里验证不 panic 及降级分支
	pb2 := &progress{w: io.Discard, label: "导出", total: 0}
	pb2.show(7)
	pb3 := &progress{w: io.Discard, label: "导出", total: 10}
	pb3.show(20) // 超出总数不 panic
}

// 回归: base URL 自带索引路径时不应重复拼接(曾出现 /idx/idx/_search)
func TestNoDoublePath(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{docs: map[string]string{"1": `{"a":1}`}})
	defer srv.Close()

	base := srv.URL + "/data_item" // 模拟用户输入的带索引路径 URL
	if err := export(base, "data_item", filepath.Join(t.TempDir(), "out.json"), exportOpts{limit: 100}); err != nil {
		t.Fatalf("export: %v", err)
	}
	fs := srv.Config.Handler.(*fakeES)
	if fs.lastPath != "/data_item/_search" {
		t.Errorf("路径重复拼接: %s", fs.lastPath)
	}
}

func TestExportParams(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{docs: map[string]string{"1": `{"a":1}`}})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "out.json")
	if err := export(srv.URL, "idx", file, exportOpts{limit: 100, searchBody: `{"query":{"term":{"status":"active"}}}`}); err != nil {
		t.Fatalf("export: %v", err)
	}
	fs := srv.Config.Handler.(*fakeES)
	if fs.lastQuery.Get("scroll") != "2m" {
		t.Errorf("scroll 参数丢失: %v", fs.lastQuery)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(fs.lastBody), &got); err != nil {
		t.Fatalf("body 非法 JSON: %v: %s", err, fs.lastBody)
	}
	if got["size"] != float64(100) {
		t.Errorf("size 未注入: %v", got["size"])
	}
	q, _ := json.Marshal(got["query"])
	if string(q) != `{"term":{"status":"active"}}` {
		t.Errorf("DSL query 未生效: %s", q)
	}
	// search-body 自带 size 时优先于 --limit
	if err := export(srv.URL, "idx", file, exportOpts{limit: 100, searchBody: `{"size":7,"query":{"match_all":{}}}`}); err != nil {
		t.Fatalf("export: %v", err)
	}
	got = nil
	if err := json.Unmarshal([]byte(fs.lastBody), &got); err != nil {
		t.Fatalf("body 非法 JSON: %v", err)
	}
	if got["size"] != float64(7) {
		t.Errorf("search-body 的 size 应优先,得到 %v", got["size"])
	}
	// DSL 非法时应报错
	if err := export(srv.URL, "idx", file, exportOpts{limit: 100, searchBody: `{"query":`}); err == nil {
		t.Error("非法 DSL 应报错")
	}
}

func TestMappingSettingsRoundTrip(t *testing.T) {
	src := &fakeES{
		mapping:  `{"idx":{"mappings":{"properties":{"name":{"type":"text"}}}}}`,
		settings: `{"idx":{"settings":{"index":{"uuid":"u1","creation_date":"1700000000000","provided_name":"idx","version":{"created":"8500003"},"number_of_replicas":"1"}}}}`,
	}
	srv := httptest.NewTLSServer(src)
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "ms.json")
	if err := exportMappingSettings(srv.URL, "idx", file, true, true); err != nil {
		t.Fatalf("export: %v", err)
	}
	b, _ := os.ReadFile(file)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("导出文件非法 JSON: %v: %s", err, b)
	}
	var parts msParts
	if err := json.Unmarshal(doc["idx"], &parts); err != nil {
		t.Fatalf("解析导出文件: %v: %s", err, b)
	}
	if parts.Mappings == nil || parts.Settings == nil {
		t.Fatalf("导出应同时含 mappings 与 settings: %s", b)
	}
	if !strings.Contains(string(parts.Settings), `"number_of_replicas"`) {
		t.Errorf("settings 应原样保留: %s", parts.Settings)
	}

	// 导入到不存在的索引: 走建索,私有设置被剔除
	dst := &fakeES{}
	srv2 := httptest.NewTLSServer(dst)
	defer srv2.Close()
	if err := importMappingSettings(file, srv2.URL, ""); err != nil { // URL 无索引名,取文件中的 idx
		t.Fatalf("import: %v", err)
	}
	cb, ok := dst.created["idx"]
	if !ok {
		t.Fatalf("应按文件中的索引名创建索引: %v", dst.created)
	}
	if strings.Contains(cb, `"uuid"`) || strings.Contains(cb, `"creation_date"`) || strings.Contains(cb, `"version"`) {
		t.Errorf("建索请求不应包含私有设置: %s", cb)
	}
	if !strings.Contains(cb, `"mappings"`) || !strings.Contains(cb, `"number_of_replicas"`) {
		t.Errorf("建索请求应包含 mappings 与可用 settings: %s", cb)
	}

	// 指定目标索引名导入
	if err := importMappingSettings(file, srv2.URL, "newidx"); err != nil {
		t.Fatalf("import newidx: %v", err)
	}
	if _, ok := dst.created["newidx"]; !ok {
		t.Fatalf("应创建索引 newidx: %v", dst.created)
	}

	// 导入到已存在的索引: 应回退为更新 _mapping 与 _settings
	dst2 := &fakeES{exists: map[string]bool{"newidx": true}}
	srv3 := httptest.NewTLSServer(dst2)
	defer srv3.Close()
	if err := importMappingSettings(file, srv3.URL, "newidx"); err != nil {
		t.Fatalf("import exists: %v", err)
	}
	if len(dst2.updated) != 2 {
		t.Fatalf("应更新 _mapping 与 _settings 各一次: %v", dst2.updated)
	}
	if !strings.Contains(dst2.updated[0], "/_mapping") || !strings.Contains(dst2.updated[0], `"properties"`) {
		t.Errorf("mapping 更新内容错误: %s", dst2.updated[0])
	}
	if !strings.Contains(dst2.updated[1], "/_settings") || strings.Contains(dst2.updated[1], `"uuid"`) {
		t.Errorf("settings 更新内容错误(私有设置应剔除): %s", dst2.updated[1])
	}
}

func TestMappingOnlyExport(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{mapping: `{"idx":{"mappings":{"properties":{"a":{"type":"long"}}}}}`})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "m.json")
	if err := exportMappingSettings(srv.URL, "idx", file, true, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	b, _ := os.ReadFile(file)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("导出文件非法 JSON: %v: %s", err, b)
	}
	var p msParts
	if err := json.Unmarshal(doc["idx"], &p); err != nil {
		t.Fatalf("解析导出文件: %v", err)
	}
	if p.Mappings == nil || p.Settings != nil {
		t.Errorf("仅导出 mapping 时不应包含 settings: %s", b)
	}
}

// 已存在索引导入: 只发送新增字段;静态设置被拒后剔除重试
func TestImportExistingSkipsRejected(t *testing.T) {
	dst := &fakeES{
		exists:         map[string]bool{"idx": true},
		mapping:        `{"idx":{"mappings":{"properties":{"old":{"type":"keyword"},"same":{"type":"long"}}}}}`,
		failStaticOnce: true,
	}
	srv := httptest.NewTLSServer(dst)
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "ms.json")
	ms := `{"idx":{"mappings":{"properties":{"old":{"type":"text"},"same":{"type":"long"},"brand_new":{"type":"keyword"}}},"settings":{"index":{"number_of_shards":"3","number_of_replicas":"0"}}}}`
	if err := os.WriteFile(file, []byte(ms), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importMappingSettings(file, srv.URL, "idx"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(dst.updated) != 3 {
		t.Fatalf("应有 1 次 mapping 与 2 次 settings 请求(首次被拒): %v", dst.updated)
	}
	if !strings.Contains(dst.updated[0], `"brand_new"`) || strings.Contains(dst.updated[0], `"old"`) {
		t.Errorf("mapping 应只含新增字段: %s", dst.updated[0])
	}
	if !strings.Contains(dst.updated[1], `"number_of_shards"`) {
		t.Errorf("首次 settings 请求应原样发送: %s", dst.updated[1])
	}
	if strings.Contains(dst.updated[2], `"number_of_shards"`) || !strings.Contains(dst.updated[2], `"number_of_replicas"`) {
		t.Errorf("重试请求应剔除静态设置: %s", dst.updated[2])
	}
}

func TestDiffMapping(t *testing.T) {
	file := []byte(`{"properties":{"a":{"type":"long"},"same":{"type":"long"},"o":{"properties":{"n":{"type":"keyword"},"x":{"type":"long"}}}}}`)
	exist := []byte(`{"properties":{"a":{"type":"keyword"},"same":{"type":"long"},"o":{"properties":{"x":{"type":"long"}}}}}`)
	got, skipped, err := diffMapping(file, exist)
	if err != nil {
		t.Fatalf("diffMapping: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("结果非法 JSON: %v", err)
	}
	props := m["properties"].(map[string]any)
	if _, ok := props["a"]; ok {
		t.Errorf("冲突字段 a 应被剔除: %s", got)
	}
	if _, ok := props["same"]; ok {
		t.Errorf("相同字段 same 应被剔除: %s", got)
	}
	o, ok := props["o"].(map[string]any)
	if !ok {
		t.Fatalf("对象字段 o 应保留其新增子字段: %s", got)
	}
	op := o["properties"].(map[string]any)
	if _, ok := op["n"]; !ok {
		t.Errorf("新增子字段 n 应保留: %s", got)
	}
	if _, ok := op["x"]; ok {
		t.Errorf("已有子字段 x 不应重复发送: %s", got)
	}
	sort.Strings(skipped)
	if strings.Join(skipped, ",") != "a,o" {
		t.Errorf("skipped 应为 [a o],得到 %v", skipped)
	}
}

func TestParseType(t *testing.T) {
	if d, m, s, err := parseType("data"); err != nil || !d || m || s {
		t.Errorf("data 解析错误: %v %v", []bool{d, m, s}, err)
	}
	if d, m, s, err := parseType("mapping,settings"); err != nil || d || !m || !s {
		t.Errorf("mapping,settings 解析错误: %v %v", []bool{d, m, s}, err)
	}
	if d, m, s, err := parseType("data,mapping,settings"); err != nil || !d || !m || !s {
		t.Errorf("三者组合解析错误: %v %v", []bool{d, m, s}, err)
	}
	if _, _, _, err := parseType("foo"); err == nil {
		t.Error("未知类型应报错")
	}
	if _, _, _, err := parseType(""); err == nil {
		t.Error("空类型应报错")
	}
}

// 多索引 mapping/settings 导出与导入
func TestMultiIndexMappingSettings(t *testing.T) {
	src := &fakeES{
		mapping:  `{"i1":{"mappings":{"properties":{"a":{"type":"long"}}}},"i2":{"mappings":{"properties":{"b":{"type":"text"}}}}}`,
		settings: `{"i1":{"settings":{"index":{"number_of_replicas":"1"}}},"i2":{"settings":{"index":{"number_of_replicas":"0"}}}}`,
	}
	srv := httptest.NewTLSServer(src)
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "multi.json")
	if err := exportMappingSettings(srv.URL, "i1,i2", file, true, true); err != nil {
		t.Fatalf("export: %v", err)
	}
	b, _ := os.ReadFile(file)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("导出文件非法 JSON: %v: %s", err, b)
	}
	if len(doc) != 2 {
		t.Fatalf("应导出 2 个索引: %s", b)
	}
	for _, name := range []string{"i1", "i2"} {
		var p msParts
		if err := json.Unmarshal(doc[name], &p); err != nil || p.Mappings == nil || p.Settings == nil {
			t.Errorf("索引 %s 应含 mappings 与 settings: %s %v", name, doc[name], err)
		}
	}

	// 导入: URL 不含索引名时应用文件中全部索引
	dst := &fakeES{}
	srv2 := httptest.NewTLSServer(dst)
	defer srv2.Close()
	if err := importMappingSettings(file, srv2.URL, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(dst.created) != 2 {
		t.Fatalf("应创建 2 个索引: %v", dst.created)
	}
	if _, ok := dst.created["i1"]; !ok {
		t.Errorf("索引 i1 未创建")
	}
	if cb := dst.created["i2"]; !strings.Contains(cb, `"b"`) {
		t.Errorf("i2 建索体应含其字段: %s", cb)
	}

	// URL 指定索引名 + 多索引文件应报错
	if err := importMappingSettings(file, srv2.URL, "target"); err == nil {
		t.Error("多索引文件指定 URL 索引名应报错")
	}
}

// 数据导出支持多索引 URL:每个文档行携带其真实 _index,导回时按元数据还原
func TestDataExportMultiIndex(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{
		docs:     map[string]string{"1": `{"a":1}`, "2": `{"b":2}`},
		docIndex: map[string]string{"1": "i1", "2": "i2"},
	})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "multi.json")
	if err := export(srv.URL, "i1,i2", file, exportOpts{limit: 100}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), `"_index":"i1"`) || !strings.Contains(string(data), `"_index":"i2"`) {
		t.Errorf("导出应携带各自 _index: %s", data)
	}

	dst := &fakeES{docs: map[string]string{}}
	srv2 := httptest.NewTLSServer(dst)
	defer srv2.Close()
	if err := importFile(file, srv2.URL, "", 100); err != nil {
		t.Fatalf("import: %v", err)
	}
	if dst.docs["1"] != "i1" || dst.docs["2"] != "i2" {
		t.Errorf("应按文档元数据还原各自索引: %v", dst.docs)
	}

	// 多索引 + 通用 search-body:同一 DSL 应用于全部匹配索引
	fs := srv.Config.Handler.(*fakeES)
	file2 := filepath.Join(t.TempDir(), "out2.json")
	if err := export(srv.URL, "i1,i2", file2, exportOpts{limit: 100, searchBody: `{"query":{"term":{"status":"active"}}}`}); err != nil {
		t.Fatalf("export with search-body: %v", err)
	}
	if fs.lastPath != "/i1,i2/_search" {
		t.Errorf("应请求多索引 _search: %s", fs.lastPath)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(fs.lastBody), &got); err != nil {
		t.Fatalf("body 非法 JSON: %v: %s", err, fs.lastBody)
	}
	q, _ := json.Marshal(got["query"])
	if string(q) != `{"term":{"status":"active"}}` {
		t.Errorf("通用 search-body 未生效: %s", q)
	}
}

// 单文件混排: 结构记录(_type=mapping/settings)在前,数据记录在后
func TestMixedExportImport(t *testing.T) {
	src := &fakeES{
		docs:     map[string]string{"1": `{"a":1}`},
		docIndex: map[string]string{"1": "i1"},
		mapping:  `{"i1":{"mappings":{"properties":{"a":{"type":"long"}}}},"i2":{"mappings":{"properties":{"b":{"type":"text"}}}}}`,
		settings: `{"i1":{"settings":{"index":{"number_of_replicas":"1"}}},"i2":{"settings":{"index":{"number_of_replicas":"0"}}}}`,
	}
	srv := httptest.NewTLSServer(src)
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "all.json")
	if err := export(srv.URL, "i1,i2", file, exportOpts{limit: 100, mapping: true, settings: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// 2 索引 x 2 条结构记录 + 1 条数据记录 = 每记录 2 行
	if len(lines) != 10 {
		t.Fatalf("期望 10 行,得到 %d: %s", len(lines), data)
	}
	if !strings.Contains(lines[0], `"_type":"mapping"`) {
		t.Errorf("结构记录应在数据之前: %s", lines[0])
	}
	if !strings.Contains(lines[len(lines)-2], `"_id":"1"`) {
		t.Errorf("数据记录应在末尾: %s", lines[len(lines)-2])
	}

	dst := &fakeES{docs: map[string]string{}}
	srv2 := httptest.NewTLSServer(dst)
	defer srv2.Close()
	if err := importFile(file, srv2.URL, "", 100); err != nil {
		t.Fatalf("import: %v", err)
	}
	// 结构应整体创建(建索请求同时含 mappings 与 settings),数据随后写入
	if len(dst.created) != 2 {
		t.Fatalf("应创建 2 个索引: %v", dst.created)
	}
	for _, name := range []string{"i1", "i2"} {
		cb := dst.created[name]
		if !strings.Contains(cb, `"mappings"`) || !strings.Contains(cb, `"number_of_replicas"`) {
			t.Errorf("索引 %s 建索请求应含 mappings 与 settings: %s", name, cb)
		}
	}
	if len(dst.updated) != 0 {
		t.Errorf("结构应一次创建完成,不应有额外更新: %v", dst.updated)
	}
	if len(dst.docs) != 1 || dst.docs["1"] != "i1" {
		t.Errorf("数据未导入: %v", dst.docs)
	}
}

// 纯结构文件不受混排逻辑影响
func TestImportStructureOnlyFile(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "ms.json")
	if err := os.WriteFile(file, []byte(`{"idx":{"mappings":{"properties":{"a":{"type":"long"}}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importMappingSettings(file, srv.URL, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	fs := srv.Config.Handler.(*fakeES)
	if fs.created["idx"] == "" {
		t.Errorf("应创建索引 idx: %v", fs.created)
	}
	// 用错 --type 时给出可操作提示
	err := importFile(file, srv.URL, "", 100)
	if err == nil || !strings.Contains(err.Error(), "--type=mapping,settings") {
		t.Errorf("应提示改用 --type=mapping,settings, 得到 %v", err)
	}
}

// --max-docs 只限制数据记录条数
func TestMaxDocs(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{docs: map[string]string{
		"1": `{"a":1}`, "2": `{"a":2}`, "3": `{"a":3}`,
	}})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "out.json")
	if err := export(srv.URL, "idx", file, exportOpts{limit: 100, maxDocs: 2}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data, _ := os.ReadFile(file)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 4 {
		t.Errorf("应导出 2 条(4 行),得到 %d 行: %s", got, data)
	}
	// 0 表示不限
	file2 := filepath.Join(t.TempDir(), "out2.json")
	if err := export(srv.URL, "idx", file2, exportOpts{limit: 100}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data2, _ := os.ReadFile(file2)
	if got := len(strings.Split(strings.TrimSpace(string(data2)), "\n")); got != 6 {
		t.Errorf("maxDocs=0 应导出全部 3 条,得到 %d 行", got)
	}
}

// 混排文件中截断只作用于数据记录,结构记录必须完整保留
func TestMaxDocsMixed(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{
		docs:     map[string]string{"1": `{"a":1}`, "2": `{"a":2}`},
		docIndex: map[string]string{"1": "i1", "2": "i2"},
		mapping:  `{"i1":{"mappings":{"properties":{"a":{"type":"long"}}}},"i2":{"mappings":{"properties":{"b":{"type":"text"}}}}}`,
		settings: `{"i1":{"settings":{"index":{"number_of_replicas":"1"}}},"i2":{"settings":{"index":{"number_of_replicas":"0"}}}}`,
	})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "all.json")
	if err := export(srv.URL, "i1,i2", file, exportOpts{limit: 100, maxDocs: 1, mapping: true, settings: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// 2 索引 x 2 条结构记录 + 1 条数据记录
	if len(lines) != 10 {
		t.Fatalf("期望 10 行(结构完整 + 1 条数据),得到 %d: %s", len(lines), data)
	}
	msRecords, dataRecords := 0, 0
	for i := 0; i < len(lines); i += 2 {
		var m metaLine
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatalf("第 %d 行非法: %v", i+1, err)
		}
		switch m.Type {
		case "mapping", "settings":
			msRecords++
		default:
			dataRecords++
		}
	}
	if msRecords != 4 || dataRecords != 1 {
		t.Errorf("结构记录应为 4、数据记录应为 1,得到 %d/%d", msRecords, dataRecords)
	}
}

// --max-docs 是多索引合计上限,不是每个索引各 N 条
func TestMaxDocsIsGlobal(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{
		docs:     map[string]string{"1": `{"a":1}`, "2": `{"a":2}`, "3": `{"a":3}`},
		docIndex: map[string]string{"1": "i1", "2": "i1", "3": "i2"},
	})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "out.json")
	if err := export(srv.URL, "i1,i2", file, exportOpts{limit: 100, maxDocs: 2}); err != nil {
		t.Fatalf("export: %v", err)
	}
	data, _ := os.ReadFile(file)
	// 每索引各 2 条会得到 6 行;合计上限 2 条应只有 4 行
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 4 {
		t.Errorf("max-docs 应为多索引合计上限(4 行),得到 %d 行: %s", got, data)
	}
}

// 计数修复: 每条记录只计一次,报错行号不再翻倍
func TestImportRecordCount(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{docs: map[string]string{}})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "bad.json")
	content := "{\"_index\":\"idx\",\"_id\":\"1\"}\n{\"a\":1}\n{\"_index\":\"idx\",\"_id\":\"2\"}\n{\"a\":2}\nnot-json\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	err := importFile(file, srv.URL, "", 100)
	if err == nil {
		t.Fatal("非法元数据应报错")
	}
	// 第 5 行元数据非法;计数翻倍时会被报成第 9 行
	if !strings.Contains(err.Error(), "第 5 行") {
		t.Errorf("行号应为 5,得到 %v", err)
	}
}

// 混排文件含多个源索引的结构时,指定目标索引名必须报错而非静默覆盖
func TestMixedMultiIndexTargetRejected(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "all.json")
	content := strings.Join([]string{
		`{"_index":"i1","_type":"mapping"}`, `{"properties":{"a":{"type":"long"}}}`,
		`{"_index":"i2","_type":"mapping"}`, `{"properties":{"b":{"type":"text"}}}`,
		`{"_index":"i1","_id":"1"}`, `{"a":1}`,
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	err := importFile(file, srv.URL, "target", 100)
	if err == nil || !strings.Contains(err.Error(), "多个索引") {
		t.Fatalf("应报多索引结构错误,得到 %v", err)
	}
	// 目标索引名恰好是其中一个源索引时同样必须报错(不能静默合并)
	if err := importFile(file, srv.URL, "i1", 100); err == nil || !strings.Contains(err.Error(), "多个索引") {
		t.Fatalf("目标索引与源索引同名时也应报错,得到 %v", err)
	}
	fs := srv.Config.Handler.(*fakeES)
	if len(fs.created) != 0 || len(fs.docs) != 0 {
		t.Errorf("报错时不应写入任何内容: created=%v docs=%v", fs.created, fs.docs)
	}

	// 单一源索引时允许重命名导入到目标索引
	single := strings.Join([]string{
		`{"_index":"i1","_type":"mapping"}`, `{"properties":{"a":{"type":"long"}}}`,
	}, "\n") + "\n"
	file2 := filepath.Join(t.TempDir(), "single.json")
	if err := os.WriteFile(file2, []byte(single), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importFile(file2, srv.URL, "target", 100); err != nil {
		t.Fatalf("单索引结构应可重命名导入: %v", err)
	}
	if fs.created["target"] == "" {
		t.Errorf("应创建索引 target: %v", fs.created)
	}
}

// 索引名来自文件时必须校验,不能把任意路径拼进 URL
func TestIndexNameValidation(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeES{})
	defer srv.Close()

	for _, name := range []string{"_cluster/settings", "_all", "../evil", "a?b=1"} {
		file := filepath.Join(t.TempDir(), "ms.json")
		body := `{"` + name + `":{"mappings":{"properties":{"a":{"type":"long"}}}}}`
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := importMappingSettings(file, srv.URL, ""); err == nil {
			t.Errorf("索引名 %q 应被拒绝", name)
		}
	}
	fs := srv.Config.Handler.(*fakeES)
	if len(fs.created) != 0 {
		t.Errorf("非法索引名不应发出建索请求: %v", fs.created)
	}
}

// mapping 除 properties 外的顶层参数(_meta 等)不能被丢弃
func TestDiffMappingKeepsTopLevel(t *testing.T) {
	file := []byte(`{"_meta":{"v":2},"dynamic":false,"properties":{"a":{"type":"long"}}}`)
	exist := []byte(`{"_meta":{"v":1},"dynamic":true,"properties":{"a":{"type":"keyword"}}}`)
	got, skipped, err := diffMapping(file, exist)
	if err != nil {
		t.Fatalf("diffMapping: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("结果非法 JSON: %v: %s", err, got)
	}
	if string(m["_meta"]) != `{"v":2}` {
		t.Errorf("_meta 应保留: %s", got)
	}
	if string(m["dynamic"]) != "false" {
		t.Errorf("dynamic 应保留: %s", got)
	}
	if _, ok := m["properties"]; ok {
		t.Errorf("冲突字段不应发送: %s", got)
	}
	if len(skipped) != 1 || skipped[0] != "a" {
		t.Errorf("skipped 应为 [a],得到 %v", skipped)
	}
}

// 导出失败不能销毁已有导出文件
func TestExportKeepsExistingFileOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(file, []byte("KEEP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := export(srv.URL, "idx", file, exportOpts{limit: 10}); err == nil {
		t.Fatal("应导出失败")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "KEEP\n" {
		t.Errorf("失败时不应改动已有文件,得到 %q", b)
	}
}
