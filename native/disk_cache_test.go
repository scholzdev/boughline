package boughline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDiskCacheBoundAcrossIndependentWriters(t *testing.T) {
	directory := t.TempDir()
	stores := []*DiskObjects{{Directory: directory, Limit: 1024}, {Directory: directory, Limit: 1024}}
	ctx := context.Background()
	var wait sync.WaitGroup
	errors := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			raw := []byte(fmt.Sprintf("data-%d", i))
			_, err := PutRaw(ctx, stores[i%2], "blob", raw)
			errors <- err
		}(i)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	if err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && !strings.HasPrefix(info.Name(), ".") {
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if total > 1024 {
		t.Fatal("cache exceeded byte budget", total)
	}
	restarted := &DiskObjects{Directory: directory, Limit: 256}
	if _, err := PutRaw(ctx, restarted, "blob", []byte("new after restart")); err != nil {
		t.Fatal(err)
	}
	if restarted.bytes > 256 || len(restarted.index) > maxDiskObjects {
		t.Fatal("restarted cache not bounded")
	}
	generation := restarted.generation
	if _, err := PutRaw(ctx, restarted, "blob", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if generation == restarted.generation {
		t.Fatal("mutation not signalled")
	}
}
