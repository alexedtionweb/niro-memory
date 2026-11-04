package niromem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func operationHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("%w: encode operation: %v", memory.ErrInvalid, err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (e *Engine) replayOperation(ctx context.Context, tenant, key, hash string, dst any) (bool, error) {
	s, ok := e.o.Store.(store.Operations)
	if !ok {
		return false, nil
	}
	r, err := s.Operation(ctx, tenant, key)
	if errors.Is(err, memory.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.OperationHash != hash {
		return false, fmt.Errorf("%w: operation key reused for different input", memory.ErrConflict)
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return false, fmt.Errorf("%w: operation result was erased", memory.ErrNotFound)
	}
	if err := json.Unmarshal(r.Result, dst); err != nil {
		return false, fmt.Errorf("decode durable operation result: %w", err)
	}
	return true, nil
}
