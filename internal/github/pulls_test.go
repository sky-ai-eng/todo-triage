package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func makePRFilesList(count int, prefix string) []map[string]any {
	files := make([]map[string]any, count)
	for i := range files {
		files[i] = map[string]any{
			"filename":  fmt.Sprintf("%s_file_%d.go", prefix, i),
			"status":    "modified",
			"additions": 1,
			"deletions": 1,
			"patch":     "@@ -1,1 +1,1 @@\n+new\n",
		}
	}
	return files
}

func TestGetPRFiles_SinglePage(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(50, "p1"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("expected 50 files, got %d", len(got))
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call, got %d", callCount)
	}
}

func TestGetPRFiles_MultiPage(t *testing.T) {
	// page 1: 100, page 2: 100, page 3: 30 → 230 total, 3 calls
	pageHits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		pageHits[page]++

		var count int
		switch page {
		case "1":
			count = 100
		case "2":
			count = 100
		case "3":
			count = 30
		default:
			t.Errorf("unexpected page %s requested", page)
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}

		data, _ := json.Marshal(makePRFilesList(count, "p"+page))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles multi-page: %v", err)
	}
	if len(got) != 230 {
		t.Errorf("expected 230 files, got %d", len(got))
	}
	for _, pg := range []string{"1", "2", "3"} {
		if pageHits[pg] != 1 {
			t.Errorf("expected 1 hit for page %s, got %d", pg, pageHits[pg])
		}
	}
}

func TestGetPRFiles_CapAt1000(t *testing.T) {
	// Every page returns 100 files; should stop after 10 pages (1000 total).
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(100, fmt.Sprintf("call%d", callCount)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles cap: %v", err)
	}
	if len(got) != maxPRFiles {
		t.Errorf("expected %d files (cap), got %d", maxPRFiles, len(got))
	}
	if callCount != maxPRFiles/100 {
		t.Errorf("expected %d API calls, got %d", maxPRFiles/100, callCount)
	}
}

func TestGetPRFiles_ErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, err := c.GetPRFiles("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestGetPRFiles_SecondPageError(t *testing.T) {
	// First page succeeds, second page fails — error should propagate.
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			data, _ := json.Marshal(makePRFilesList(100, "p1"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, err := c.GetPRFiles("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error when second page fails, got nil")
	}
}
