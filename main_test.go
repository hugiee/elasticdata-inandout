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
	"strings"
	"testing"
)

// 假 ES:内存 map 按 _id 存文档,支持 scroll search 与 _bulk
type fakeES struct {
	docs      map[string]string
	lastPath  string     // 最近一次 _search 请求路径
	lastBulk  string     // 最近一次 _bulk 请求体
	lastQuery url.Values // 最近一次 _search 的查询参数
	lastBody  string     // 最近一次 _search 的请求体
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
			fmt.Fprintf(w, `{"_index":"idx","_id":%q,"_source":%s}`, id, f.docs[id])
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
	if err := export(srv.URL, "idx", file, 100, ""); err != nil {
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
	if err := export(base, "data_item", filepath.Join(t.TempDir(), "out.json"), 100, ""); err != nil {
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
	if err := export(srv.URL, "idx", file, 100, `{"query":{"term":{"status":"active"}}}`); err != nil {
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
	// DSL 非法时应报错
	if err := export(srv.URL, "idx", file, 100, `{"query":`); err == nil {
		t.Error("非法 DSL 应报错")
	}
}
