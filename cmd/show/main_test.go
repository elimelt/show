// Tests for show server streaming and replay.
// Author: Elijah Melton
package main

import (
	"bufio"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readSSEChunks(t *testing.T, client *http.Client, url string, lastID string, maxBytes int) ([]byte, bool) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	var buf []byte
	var haveDone bool
	var curEvent, curData, curType string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read error: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" { // dispatch event
			if curEvent == "chunk" && curData != "" {
				dec, _ := io.ReadAll(base64NewDecoder(curData))
				buf = append(buf, dec...)
				if maxBytes > 0 && len(buf) >= maxBytes {
					return buf, haveDone
				}
			}
			if curType == "done" {
				haveDone = true
				return buf, haveDone
			}
			curEvent, curData, curType = "", "", ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if curEvent == "done" {
				curType = "done"
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			curData += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			continue
		}
	}
	return buf, haveDone
}

func base64NewDecoder(s string) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(s))
}

func TestBacklogReplay(t *testing.T) {
	srv := newStreamServer(1 << 20)
	expect := []byte("hello world\n")
	srv.append(expect)
	ts := httptest.NewServer(newHandler(srv, "test"))
	defer ts.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	got, done := readSSEChunks(t, client, ts.URL+"/stream", "", len(expect))
	if string(got) != string(expect) {
		t.Fatalf("backlog mismatch: got %q want %q", string(got), string(expect))
	}
	if done {
		t.Fatalf("unexpected done event")
	}
}

func TestResumeAfterID(t *testing.T) {
	srv := newStreamServer(1 << 20)
	srv.append([]byte("abc"))
	srv.append([]byte("def"))
	ts := httptest.NewServer(newHandler(srv, "test"))
	defer ts.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	got, _ := readSSEChunks(t, client, ts.URL+"/stream", "3", 3)
	if string(got) != "def" {
		t.Fatalf("resume mismatch: got %q want %q", string(got), "def")
	}
}

func TestRingBufferClamp(t *testing.T) {
	srv := newStreamServer(5)
	srv.append([]byte("1234567890"))
	ts := httptest.NewServer(newHandler(srv, "test"))
	defer ts.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	got, _ := readSSEChunks(t, client, ts.URL+"/stream", "3", 5)
	if string(got) != "67890" {
		t.Fatalf("clamp mismatch: got %q want %q", string(got), "67890")
	}
}

func TestDoneOnConnect(t *testing.T) {
	srv := newStreamServer(1 << 20)
	srv.append([]byte("hi"))
	srv.finish()
	ts := httptest.NewServer(newHandler(srv, "test"))
	defer ts.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	got, done := readSSEChunks(t, client, ts.URL+"/stream", "", 0)
	if string(got) != "hi" || !done {
		t.Fatalf("done/backlog mismatch: got %q done=%v", string(got), done)
	}
}
