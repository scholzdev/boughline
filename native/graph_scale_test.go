package boughline

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

type graphTestObjects struct {
	mu      sync.RWMutex
	objects map[string][]byte
	reads   atomic.Int64
}

func (s *graphTestObjects) Get(ctx context.Context, kind, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := objectKey(kind, id)
	if err != nil {
		return nil, err
	}
	s.reads.Add(1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, fail(404, "object not found")
	}
	return append([]byte{}, data...), nil
}
func (s *graphTestObjects) Put(ctx context.Context, kind, id string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := objectKey(kind, id)
	if err != nil {
		return err
	}
	if _, err = Unpack(data, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte{}, data...)
	return nil
}
func graphFixture(t *testing.T) (*fixture, *graphTestObjects) {
	t.Helper()
	store := &graphTestObjects{objects: map[string][]byte{}}
	f := &fixture{ctx: context.Background(), p: Principal{Name: "owner", Admin: true}}
	f.e = NewEngine(NewMemoryMetadata(), store)
	var err error
	f.genesis, err = f.e.CreateRepo(f.ctx, RepoConfig{ID: "test", Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return f, store
}
func TestMergeBaseDoesNotWalkUnrelatedMainHistory(t *testing.T) {
	f, store := graphFixture(t)
	head := f.genesis
	for i := 0; i < 1024; i++ {
		var err error
		head, err = f.e.Commit(f.ctx, "test", f.p, CommitRequest{Parents: []string{head}, Changes: map[string]*File{}, Message: "history", RequestID: fmt.Sprintf("history-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	branch := f.commit(t, f.genesis, map[string]*File{})
	store.reads.Store(0)
	bases, err := f.e.MergeBases(f.ctx, "test", head, branch)
	if err != nil || len(bases) != 1 || bases[0] != f.genesis {
		t.Fatal(bases, err)
	}
	t.Logf("merge-base object reads across 1024 unrelated commits: %d", store.reads.Load())
	if reads := store.reads.Load(); reads > 128 {
		t.Fatalf("one unlanded change walked unrelated history: %d object reads", reads)
	}
}

func TestReachabilityAndMergeBasesMatchIndependentDAG(t *testing.T) {
	f, _ := graphFixture(t)
	rng := rand.New(rand.NewPCG(23, 91))
	ids := []string{f.genesis}
	parents := map[string][]string{f.genesis: {}}
	for i := 0; i < 100; i++ {
		a := ids[rng.IntN(len(ids))]
		ps := []string{a}
		if i > 2 && rng.IntN(2) == 0 {
			b := ids[rng.IntN(len(ids))]
			if a != b {
				ps = append(ps, b)
			}
		}
		id, err := f.e.Commit(f.ctx, "test", f.p, CommitRequest{Parents: ps, Changes: map[string]*File{}, Message: "DAG", RequestID: fmt.Sprintf("dag-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		parents[id] = ps
	}
	closure := map[string]map[string]bool{}
	for _, id := range ids {
		set := map[string]bool{}
		todo := []string{id}
		for len(todo) > 0 {
			x := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if set[x] {
				continue
			}
			set[x] = true
			todo = append(todo, parents[x]...)
		}
		closure[id] = set
	}
	for i := 0; i < 100; i++ {
		a, b := ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))]
		yes, err := f.e.Ancestor(f.ctx, "test", a, b)
		if err != nil || yes != closure[b][a] {
			t.Fatal("reachability differs from DAG", yes, err)
		}
		common := []string{}
		for id := range closure[a] {
			if closure[b][id] {
				common = append(common, id)
			}
		}
		want := []string{}
		for _, candidate := range common {
			maximal := true
			for _, other := range common {
				if candidate != other && closure[other][candidate] {
					maximal = false
					break
				}
			}
			if maximal {
				want = append(want, candidate)
			}
		}
		sort.Strings(want)
		got, err := f.e.MergeBases(f.ctx, "test", a, b)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("merge bases differ: want %v got %v err %v", want, got, err)
		}
	}
}
