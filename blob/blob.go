// Package blob defines content-addressed storage for binary episode parts
// (images, audio, video, files) and two built-in stores: Dir (local
// directory) and Mem (in-process, for tests). The S3 store is a connector.
//
// Blobs are keyed by tenant and SHA-256, so identical bytes are stored once
// per tenant and one tenant can never read another tenant's blob by hash.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/alexedtionweb/niro-memory/memory"
)

// Store is the connector interface for blob storage.
type Store interface {
	Put(ctx context.Context, tenant string, data []byte, mime string) (memory.BlobRef, error)
	Get(ctx context.Context, tenant, sha256 string) ([]byte, error)
	Delete(ctx context.Context, tenant, sha256 string) error
}

// Ref computes the content address of data.
func Ref(data []byte, mime string) memory.BlobRef {
	sum := sha256.Sum256(data)
	return memory.BlobRef{SHA256: hex.EncodeToString(sum[:]), Mime: mime, Size: int64(len(data))}
}

// ValidKey reports whether tenant and sha are safe to use in paths and keys.
func ValidKey(tenant, sha string) error {
	if len(sha) != 64 {
		return fmt.Errorf("%w: bad blob hash", memory.ErrInvalid)
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("%w: bad blob hash", memory.ErrInvalid)
		}
	}
	if tenant == "" || len(tenant) > 128 {
		return fmt.Errorf("%w: bad tenant", memory.ErrInvalid)
	}
	for _, c := range tenant {
		if c == '/' || c == '\\' || c == 0 || tenant == "." || tenant == ".." {
			return fmt.Errorf("%w: bad tenant", memory.ErrInvalid)
		}
	}
	return nil
}

// Mem is an in-process Store for tests and demos.
type Mem struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewMem returns an empty in-process blob store.
func NewMem() *Mem { return &Mem{data: map[string][]byte{}} }

// Put stores data and returns its reference.
func (m *Mem) Put(_ context.Context, tenant string, data []byte, mime string) (memory.BlobRef, error) {
	ref := Ref(data, mime)
	m.mu.Lock()
	m.data[tenant+"/"+ref.SHA256] = append([]byte(nil), data...)
	m.mu.Unlock()
	return ref, nil
}

// Get returns the bytes of a blob or memory.ErrNotFound.
func (m *Mem) Get(_ context.Context, tenant, sha string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[tenant+"/"+sha]
	if !ok {
		return nil, memory.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

// Delete removes a blob; deleting a missing blob is not an error.
func (m *Mem) Delete(_ context.Context, tenant, sha string) error {
	m.mu.Lock()
	delete(m.data, tenant+"/"+sha)
	m.mu.Unlock()
	return nil
}

// Dir stores blobs as files under root/<tenant>/<sha[0:2]>/<sha>.
// Writes go to a temp file and are renamed, so readers never see partial data.
type Dir struct{ Root string }

func (d Dir) path(tenant, sha string) string {
	return filepath.Join(d.Root, tenant, sha[:2], sha)
}

// Put writes data if it is not already stored.
func (d Dir) Put(_ context.Context, tenant string, data []byte, mime string) (memory.BlobRef, error) {
	ref := Ref(data, mime)
	if err := ValidKey(tenant, ref.SHA256); err != nil {
		return ref, err
	}
	p := d.path(tenant, ref.SHA256)
	if _, err := os.Stat(p); err == nil {
		return ref, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return ref, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return ref, err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return ref, err
	}
	return ref, os.Rename(tmp.Name(), p)
}

// Get reads a blob or returns memory.ErrNotFound.
func (d Dir) Get(_ context.Context, tenant, sha string) ([]byte, error) {
	if err := ValidKey(tenant, sha); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(d.path(tenant, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil, memory.ErrNotFound
	}
	return b, err
}

// Delete removes a blob; deleting a missing blob is not an error.
func (d Dir) Delete(_ context.Context, tenant, sha string) error {
	if err := ValidKey(tenant, sha); err != nil {
		return err
	}
	err := os.Remove(d.path(tenant, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
