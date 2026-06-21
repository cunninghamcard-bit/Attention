package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/config"
)

const (
	lockAttempts = 50
	lockDelay    = 20 * time.Millisecond
	// pi's proper-lockfile treats locks older than this as stale and steals
	// them (auth-storage.ts:136-149), so a crashed process never wedges auth.
	lockStaleAfter = 30 * time.Second
)

type Store interface {
	Get(provider string) (Credential, bool)
	Set(ctx context.Context, provider string, cred Credential) error
	Delete(ctx context.Context, provider string) error
}

type FileStore struct {
	path string
}

func NewStore(path string) (*FileStore, error) {
	if path == "" {
		authPath, err := config.AuthJSONPath()
		if err != nil {
			return nil, err
		}
		path = authPath
	}

	return &FileStore{
		path: filepath.Clean(path),
	}, nil
}

func (s *FileStore) Path() string {
	return s.path
}

func (s *FileStore) Get(provider string) (Credential, bool) {
	cred, ok, _ := s.GetError(provider)
	return cred, ok
}

func (s *FileStore) GetError(provider string) (Credential, bool, error) {
	if provider == "" {
		return Credential{}, false, nil
	}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return Credential{}, false, nil
	}

	var cred Credential
	var ok bool
	err := s.withLock(context.Background(), func() error {
		data, err := s.readData()
		if err != nil {
			return err
		}
		cred, ok = data[provider]
		return nil
	})
	if err != nil {
		return Credential{}, false, err
	}
	return cred, ok, nil
}

func (s *FileStore) Set(ctx context.Context, provider string, cred Credential) error {
	if provider == "" {
		return fmt.Errorf("provider is required")
	}
	if err := validateCredential(cred); err != nil {
		return err
	}

	return s.withLock(ctx, func() error {
		data, err := s.readData()
		if err != nil {
			return err
		}
		data[provider] = cred
		return s.writeData(data)
	})
}

// UpdateLocked runs fn with the cross-process file lock held, giving it the
// current data and persisting whatever it returns (nil means no write). pi's
// withLockAsync holds the lock across the whole read-refresh-persist sequence
// (auth-storage.ts:415-448).
func (s *FileStore) UpdateLocked(
	ctx context.Context,
	fn func(data map[string]Credential) (map[string]Credential, error),
) error {
	return s.withLock(ctx, func() error {
		data, err := s.readData()
		if err != nil {
			return err
		}
		next, err := fn(data)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		return s.writeData(next)
	})
}

func (s *FileStore) Delete(ctx context.Context, provider string) error {
	if provider == "" {
		return fmt.Errorf("provider is required")
	}

	return s.withLock(ctx, func() error {
		data, err := s.readData()
		if err != nil {
			return err
		}
		delete(data, provider)
		return s.writeData(data)
	})
}

func (s *FileStore) withLock(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create auth directory %s: %w", filepath.Dir(s.path), err)
	}

	lock, err := acquireFileLock(ctx, s.path+".lock")
	if err != nil {
		return err
	}
	defer lock.release()

	return fn()
}

func (s *FileStore) readData() (map[string]Credential, error) {
	data := map[string]Credential{}
	content, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return data, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read auth storage %s: %w", s.path, err)
	}
	if len(content) == 0 {
		return data, nil
	}
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("parse auth storage %s: %w", s.path, err)
	}
	if data == nil {
		return map[string]Credential{}, nil
	}
	return data, nil
}

func (s *FileStore) writeData(data map[string]Credential) error {
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth storage %s: %w", s.path, err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(s.path, content, 0o600); err != nil {
		return fmt.Errorf("write auth storage %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("chmod auth storage %s: %w", s.path, err)
	}
	return nil
}

type fileLock struct {
	file *os.File
	path string
}

func acquireFileLock(ctx context.Context, path string) (*fileLock, error) {
	var lastErr error
	for attempt := range lockAttempts {
		lockFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(lockFile, "%d\n", os.Getpid())
			return &fileLock{
				file: lockFile,
				path: path,
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire auth lock %s: %w", path, err)
		}
		lastErr = err

		// Steal locks left behind by crashed processes.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}

		if attempt+1 == lockAttempts {
			break
		}
		timer := time.NewTimer(lockDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	return nil, fmt.Errorf("acquire auth lock %s: timed out: %w", path, lastErr)
}

func (l *fileLock) release() {
	_ = l.file.Close()
	_ = os.Remove(l.path)
}
