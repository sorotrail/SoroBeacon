// Package archivetest holds an in-memory object store for tests, so archive
// behaviour is exercised without a bucket or a filesystem.
package archivetest

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/sorotrail/sorobeacon/internal/archive"
)

// Fake is an in-memory archive.ObjectStore.
type Fake struct {
	mu   sync.Mutex
	objs map[string][]byte
	// MutatePut, when set, rewrites an object's bytes as it is written, to
	// simulate a backend that silently corrupts what it stores.
	MutatePut func(key string, body []byte) []byte
	Gets      int
}

// New returns an empty Fake.
func New() *Fake { return &Fake{objs: map[string][]byte{}} }

// Archive stores the object, overwriting any existing one.
func (f *Fake) Archive(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MutatePut != nil {
		b = f.MutatePut(key, b)
	}
	f.objs[key] = b
	return nil
}

// Get returns the object or archive.ErrObjectNotFound.
func (f *Fake) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Gets++
	b, ok := f.objs[key]
	if !ok {
		return nil, archive.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Put sets an object directly (to plant a corrupt one).
func (f *Fake) Put(key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = b
}

// Delete removes an object.
func (f *Fake) Delete(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, key)
}

// Keys lists stored keys with the prefix, sorted.
func (f *Fake) Keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

var _ archive.ObjectStore = (*Fake)(nil)
