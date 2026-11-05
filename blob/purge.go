package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func purgeKey(tenant string, limit int) error {
	if limit < 1 || limit > 1000 {
		return fmt.Errorf("blob: purge batch must be 1..1000")
	}
	return ValidKey(tenant, strings.Repeat("0", 64))
}

// PurgeTenantBatch removes a bounded number of this tenant's objects.
func (m *Mem) PurgeTenantBatch(ctx context.Context, tenant string, limit int) (int, bool, error) {
	if err := purgeKey(tenant, limit); err != nil {
		return 0, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range m.data {
		if err := ctx.Err(); err != nil {
			return n, false, err
		}
		if strings.HasPrefix(key, tenant+"/") {
			if n == limit {
				return n, false, nil
			}
			delete(m.data, key)
			n++
		}
	}
	return n, true, nil
}

// PurgeTenantBatch unlinks individual files, never recursively deleting paths.
func (d Dir) PurgeTenantBatch(ctx context.Context, tenant string, limit int) (int, bool, error) {
	if err := purgeKey(tenant, limit); err != nil {
		return 0, false, err
	}
	root := filepath.Join(d.Root, tenant)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, false, fmt.Errorf("blob: unsafe tenant directory")
	}
	n := 0
	for prefix := 0; prefix < 256; prefix++ {
		if err := ctx.Err(); err != nil {
			return n, false, err
		}
		dir := filepath.Join(root, fmt.Sprintf("%02x", prefix))
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return n, false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return n, false, fmt.Errorf("blob: unsafe blob directory")
		}
		f, err := os.Open(dir)
		if err != nil {
			return n, false, err
		}
		names, readErr := f.Readdirnames(limit - n + 1)
		closeErr := f.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return n, false, readErr
		}
		if closeErr != nil {
			return n, false, closeErr
		}
		for _, name := range names {
			if n == limit {
				return n, false, nil
			}
			if err := ctx.Err(); err != nil {
				return n, false, err
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return n, false, err
			}
			n++
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return n, false, err
		}
	}
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return n, false, err
	}
	return n, true, nil
}
