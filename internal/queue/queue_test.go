package queue

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type job struct {
	Text     string `json:"text"`
	Attempts int    `json:"attempts"`
}

func newDir(t *testing.T) Dir {
	t.Helper()
	d := Dir{Root: filepath.Join(t.TempDir(), "q")}
	if err := d.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return d
}

func mustPending(t *testing.T, d Dir) []string {
	t.Helper()
	ids, err := d.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	return ids
}

func TestInit_建立兩個子目錄且可重複呼叫(t *testing.T) {
	d := newDir(t)
	if err := d.Init(); err != nil {
		t.Fatalf("第二次 Init 應該冪等，實際錯誤: %v", err)
	}
	for _, p := range []string{d.pending(), d.done()} {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			t.Errorf("Init 後 %s 應該是目錄 (err=%v)", p, err)
		}
	}
}

func TestPutGet_內容原樣讀回且出現在Pending(t *testing.T) {
	d := newDir(t)
	want := job{Text: "哈囉 \"world\"\n換行", Attempts: 3}
	if err := d.Put("a1", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := mustPending(t, d); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Fatalf("Pending = %v，應為 [a1]", got)
	}
	var got job
	if err := d.Get("a1", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Errorf("Get 讀回 %+v，應為 %+v", got, want)
	}
}

func TestPut_成功後不留暫存檔(t *testing.T) {
	d := newDir(t)
	if err := d.Put("a1", job{}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(d.pending())
	if len(ents) != 1 || ents[0].Name() != "a1.json" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("pending/ 應該只有 a1.json，實際 %v", names)
	}
}

func TestPut_重複id在pending時回ErrExists且不覆寫(t *testing.T) {
	d := newDir(t)
	if err := d.Put("dup", job{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	err := d.Put("dup", job{Text: "second"})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("重複 Put 回傳 %v，應為 ErrExists", err)
	}
	var got job
	if err := d.Get("dup", &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "first" {
		t.Errorf("重複 Put 不應覆寫原內容，讀到 %q", got.Text)
	}
}

func TestPut_重複id在done時回ErrExists且不回到pending(t *testing.T) {
	d := newDir(t)
	if err := d.Put("dup", job{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Done("dup"); err != nil {
		t.Fatal(err)
	}
	err := d.Put("dup", job{})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("已處理過的 id 再 Put 回傳 %v，應為 ErrExists（否則同訊息會被處理兩次）", err)
	}
	if got := mustPending(t, d); len(got) != 0 {
		t.Errorf("被拒絕的 Put 不應讓 job 回到 pending，實際 %v", got)
	}
}

func TestPut_非法id被拒且不寫任何檔(t *testing.T) {
	cases := []string{"", "a/b", "../escape", `a\b`, "/abs"}
	for _, id := range cases {
		d := newDir(t)
		err := d.Put(id, job{})
		if err == nil {
			t.Errorf("id %q 應被拒絕，卻成功了", id)
			continue
		}
		if errors.Is(err, ErrExists) {
			t.Errorf("id %q 是非法而非已存在，不應回 ErrExists", id)
		}
		if got := mustPending(t, d); len(got) != 0 {
			t.Errorf("id %q 被拒後 pending 應為空，實際 %v", id, got)
		}
		if _, err := os.Stat(filepath.Join(d.Root, "escape.json")); err == nil {
			t.Errorf("id %q 逃出了 pending 目錄", id)
		}
	}
}

func TestPut_無法序列化的內容回錯誤且不留檔(t *testing.T) {
	d := newDir(t)
	if err := d.Put("bad", make(chan int)); err == nil {
		t.Fatal("chan 無法 JSON 序列化，Put 應回錯誤")
	}
	if got := mustPending(t, d); len(got) != 0 {
		t.Errorf("序列化失敗不應產生 job，實際 %v", got)
	}
	if err := d.Put("bad", job{}); err != nil {
		t.Errorf("失敗後同 id 應可重新 Put: %v", err)
	}
}

func TestPut_沒有Init時回錯誤(t *testing.T) {
	d := Dir{Root: filepath.Join(t.TempDir(), "nope")}
	if err := d.Put("a", job{}); err == nil {
		t.Error("目錄不存在時 Put 應回錯誤")
	}
	if _, err := d.Pending(); err == nil {
		t.Error("目錄不存在時 Pending 應回錯誤")
	}
}

func TestPending_空佇列回空(t *testing.T) {
	d := newDir(t)
	if got := mustPending(t, d); len(got) != 0 {
		t.Errorf("新佇列 Pending 應為空，實際 %v", got)
	}
}

func TestPending_忽略暫存檔_子目錄_非json且順序穩定(t *testing.T) {
	d := newDir(t)
	// 故意亂序寫入；id 以時間開頭時應照字典序輸出
	for _, id := range []string{"20260102-b", "20260101-z", "20260101-a", "20260103-c"} {
		if err := d.Put(id, job{}); err != nil {
			t.Fatal(err)
		}
	}
	// 模擬 Put/Update 中途崩潰留下的暫存檔，以及雜物
	junk := []string{".20260101-q.json.tmp", "note.txt", "20260101-x.json.tmp"}
	for _, n := range junk {
		if err := os.WriteFile(filepath.Join(d.pending(), n), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(d.pending(), "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{"20260101-a", "20260101-z", "20260102-b", "20260103-c"}
	for i := 0; i < 3; i++ {
		if got := mustPending(t, d); !reflect.DeepEqual(got, want) {
			t.Fatalf("第 %d 次 Pending = %v，應為 %v", i, got, want)
		}
	}
}

func TestDone_從pending搬到done(t *testing.T) {
	d := newDir(t)
	want := job{Text: "x", Attempts: 2}
	for _, id := range []string{"a", "b"} {
		if err := d.Put(id, want); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Done("a"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if got := mustPending(t, d); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("Done(a) 後 Pending = %v，應只剩 [b]", got)
	}
	if _, err := os.Stat(filepath.Join(d.done(), "a.json")); err != nil {
		t.Errorf("done/a.json 應存在: %v", err)
	}
	var got job
	if err := d.Get("a", &got); err == nil {
		t.Error("已 Done 的 job 不應再能用 Get 讀到")
	}
}

func TestDone_不存在的id回錯誤(t *testing.T) {
	d := newDir(t)
	if err := d.Done("ghost"); err == nil {
		t.Error("對不存在的 job 呼叫 Done 應回錯誤")
	}
	if err := d.Put("x", job{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Done("x"); err != nil {
		t.Fatal(err)
	}
	if err := d.Done("x"); err == nil {
		t.Error("同一個 job 第二次 Done 應回錯誤")
	}
}

func TestGet_不存在的id回IsNotExist錯誤(t *testing.T) {
	d := newDir(t)
	var v job
	err := d.Get("ghost", &v)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Get 不存在的 id 回傳 %v，應為 ErrNotExist", err)
	}
}

func TestGet_檔案內容損毀時回錯誤(t *testing.T) {
	d := newDir(t)
	if err := os.WriteFile(filepath.Join(d.pending(), "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var v job
	if err := d.Get("broken", &v); err == nil {
		t.Error("損毀的 JSON 應回錯誤，而不是靜默成功")
	}
}

func TestUpdate_改寫內容且不增加job也不留暫存檔(t *testing.T) {
	d := newDir(t)
	if err := d.Put("u", job{Text: "t", Attempts: 0}); err != nil {
		t.Fatal(err)
	}
	if err := d.Update("u", job{Text: "t", Attempts: 5}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var got job
	if err := d.Get("u", &got); err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 5 {
		t.Errorf("Update 後 Attempts = %d，應為 5", got.Attempts)
	}
	if ids := mustPending(t, d); !reflect.DeepEqual(ids, []string{"u"}) {
		t.Errorf("Update 後 Pending = %v，應仍為 [u]", ids)
	}
	ents, _ := os.ReadDir(d.pending())
	if len(ents) != 1 {
		t.Errorf("pending/ 應只有 1 個檔，實際 %d（暫存檔沒清掉？）", len(ents))
	}
}

func TestUpdate_無法序列化時保留原內容(t *testing.T) {
	d := newDir(t)
	if err := d.Put("u", job{Text: "orig"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Update("u", make(chan int)); err == nil {
		t.Fatal("無法序列化應回錯誤")
	}
	var got job
	if err := d.Get("u", &got); err != nil || got.Text != "orig" {
		t.Errorf("Update 失敗後原內容應保留，got=%+v err=%v", got, err)
	}
}

func TestCrashRecovery_只有pending檔重建Dir後仍列得到(t *testing.T) {
	d := newDir(t)
	for _, id := range []string{"1", "2", "3"} {
		if err := d.Put(id, job{Text: "t" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Done("1"); err != nil {
		t.Fatal(err)
	}
	// 模擬崩潰：丟掉舊的 Dir，只用同一個 Root 重新建立
	d2 := Dir{Root: d.Root}
	if err := d2.Init(); err != nil {
		t.Fatalf("重啟後 Init 不應破壞既有資料: %v", err)
	}
	if got := mustPending(t, d2); !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Fatalf("重啟後 Pending = %v，應為 [2 3]", got)
	}
	var v job
	if err := d2.Get("3", &v); err != nil || v.Text != "t3" {
		t.Errorf("重啟後 Get(3) = %+v, err=%v", v, err)
	}
	// 去重資訊也要跨重啟存活（done 與 pending 都是）
	for _, id := range []string{"1", "2"} {
		if err := d2.Put(id, job{}); !errors.Is(err, ErrExists) {
			t.Errorf("重啟後重送 id=%s 回傳 %v，應為 ErrExists", id, err)
		}
	}
}
