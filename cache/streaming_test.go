package cache

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestStreamingEntryConcurrentReaders(t *testing.T) {
	header := http.Header{"Content-Type": {"application/octet-stream"}}
	se := NewStreamingEntry(header, 200)

	const numReaders = 5
	var wg sync.WaitGroup
	results := make([][]byte, numReaders)

	// Start readers before any data is written.
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reader := se.NewReader(0)
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Errorf("reader %d error: %v", idx, err)
				return
			}
			results[idx] = data
		}(i)
	}

	// Write data in chunks.
	chunks := []string{"Hello, ", "World! ", "This is ", "streaming."}
	for _, chunk := range chunks {
		time.Sleep(5 * time.Millisecond) // simulate slow origin
		if _, err := se.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write error: %v", err)
		}
	}
	se.Close(nil)

	wg.Wait()

	want := []byte("Hello, World! This is streaming.")
	for i, got := range results {
		if !bytes.Equal(got, want) {
			t.Errorf("reader %d: got %q, want %q", i, got, want)
		}
	}
}

func TestStreamingReaderBlocksUntilData(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)
	reader := se.NewReader(0)

	// Reader should block because no data is available.
	readDone := make(chan struct{})
	var buf [64]byte
	var n int
	var readErr error

	go func() {
		n, readErr = reader.Read(buf[:])
		close(readDone)
	}()

	// Give the reader goroutine time to block.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-readDone:
		t.Fatal("Read returned before data was written")
	default:
		// Good, still blocking.
	}

	// Write some data — reader should unblock.
	se.Write([]byte("data"))

	select {
	case <-readDone:
		if readErr != nil {
			t.Fatalf("unexpected error: %v", readErr)
		}
		if n != 4 {
			t.Fatalf("expected 4 bytes, got %d", n)
		}
		if string(buf[:n]) != "data" {
			t.Fatalf("got %q, want %q", string(buf[:n]), "data")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Write")
	}

	se.Close(nil)
}

func TestStreamingReaderEOFAfterClose(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)

	se.Write([]byte("all data"))
	se.Close(nil)

	reader := se.NewReader(0)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "all data" {
		t.Fatalf("got %q, want %q", data, "all data")
	}
}

func TestStreamingReaderErrorPropagation(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)

	se.Write([]byte("partial"))
	originErr := errors.New("connection reset")
	se.Close(originErr)

	reader := se.NewReader(0)
	data := make([]byte, 100)

	// First read returns the buffered data.
	n, err := reader.Read(data)
	if err != nil {
		t.Fatalf("first read should succeed, got error: %v", err)
	}
	if string(data[:n]) != "partial" {
		t.Fatalf("got %q, want %q", string(data[:n]), "partial")
	}

	// Second read should return the origin error.
	_, err = reader.Read(data)
	if !errors.Is(err, originErr) {
		t.Fatalf("expected origin error, got: %v", err)
	}
}

func TestStreamingReaderWithOffset(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)

	se.Write([]byte("0123456789"))
	se.Close(nil)

	reader := se.NewReader(5)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "56789" {
		t.Fatalf("got %q, want %q", data, "56789")
	}
}

func TestStreamingEntryToEntry(t *testing.T) {
	header := http.Header{
		"Content-Type": {"text/plain"},
		"X-Custom":     {"value"},
	}
	se := NewStreamingEntry(header, 200)

	se.Write([]byte("full body content"))
	se.Close(nil)

	entry := se.ToEntry(60*time.Second, 30*time.Second, `"abc123"`, "Mon, 01 Jan 2024 00:00:00 GMT", "example.com/path")

	if entry == nil {
		t.Fatal("ToEntry returned nil")
	}
	if string(entry.Body) != "full body content" {
		t.Fatalf("body: got %q, want %q", entry.Body, "full body content")
	}
	if entry.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", entry.StatusCode)
	}
	if entry.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("header Content-Type: got %q", entry.Header.Get("Content-Type"))
	}
	if entry.TTL != 60*time.Second {
		t.Fatalf("TTL: got %v, want 60s", entry.TTL)
	}
	if entry.StaleWhileRevalidate != 30*time.Second {
		t.Fatalf("SWR: got %v, want 30s", entry.StaleWhileRevalidate)
	}
	if entry.ETag != `"abc123"` {
		t.Fatalf("ETag: got %q", entry.ETag)
	}
	if entry.LastMod != "Mon, 01 Jan 2024 00:00:00 GMT" {
		t.Fatalf("LastMod: got %q", entry.LastMod)
	}
	if entry.VaryKey != "example.com/path" {
		t.Fatalf("VaryKey: got %q", entry.VaryKey)
	}
}

func TestStreamingEntryToEntryWithError(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)
	se.Write([]byte("partial"))
	se.Close(errors.New("failed"))

	entry := se.ToEntry(time.Minute, 0, "", "", "")
	if entry != nil {
		t.Fatal("ToEntry should return nil when closed with error")
	}
}

func TestStreamingEntryIsComplete(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)

	if se.IsComplete() {
		t.Fatal("should not be complete before Close")
	}

	se.Close(nil)

	if !se.IsComplete() {
		t.Fatal("should be complete after Close")
	}
}

func TestStreamingEntryWriteAfterClose(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)
	se.Close(nil)

	_, err := se.Write([]byte("late"))
	if err == nil {
		t.Fatal("Write after Close should return error")
	}
}

func TestStreamingEntryMultipleReadersAtDifferentSpeeds(t *testing.T) {
	se := NewStreamingEntry(http.Header{}, 200)

	var wg sync.WaitGroup

	// Fast reader — reads everything in one go.
	wg.Add(1)
	go func() {
		defer wg.Done()
		reader := se.NewReader(0)
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Errorf("fast reader error: %v", err)
			return
		}
		if string(data) != "AABB" {
			t.Errorf("fast reader: got %q, want %q", data, "AABB")
		}
	}()

	// Slow reader — starts after first chunk.
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Wait for first chunk.
		time.Sleep(30 * time.Millisecond)
		reader := se.NewReader(0)
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Errorf("slow reader error: %v", err)
			return
		}
		if string(data) != "AABB" {
			t.Errorf("slow reader: got %q, want %q", data, "AABB")
		}
	}()

	se.Write([]byte("AA"))
	time.Sleep(20 * time.Millisecond)
	se.Write([]byte("BB"))
	se.Close(nil)

	wg.Wait()
}
