package builtin

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type fileMutationQueue struct {
	mu   sync.Mutex
	refs int
}

var fileMutationQueues = struct {
	sync.Mutex
	queues map[string]*fileMutationQueue
}{
	queues: map[string]*fileMutationQueue{},
}

// withFileMutationQueue serializes file mutation operations targeting the same
// physical file. The path is resolved via EvalSymlinks so that two paths
// reaching the same file through different symlinks share a single queue,
// mirroring pi's getMutationQueueKey (fs.realpath fallback to resolve).
func withFileMutationQueue[T any](path string, fn func() T) (T, error) {
	key, err := mutationQueueKey(path)
	if err != nil {
		var zero T
		return zero, err
	}

	fileMutationQueues.Lock()
	queue := fileMutationQueues.queues[key]
	if queue == nil {
		queue = &fileMutationQueue{}
		fileMutationQueues.queues[key] = queue
	}
	queue.refs++
	fileMutationQueues.Unlock()

	queue.mu.Lock()
	defer releaseFileMutationQueue(key, queue)
	return fn(), nil
}

func releaseFileMutationQueue(key string, queue *fileMutationQueue) {
	queue.mu.Unlock()

	fileMutationQueues.Lock()
	defer fileMutationQueues.Unlock()

	queue.refs--
	if queue.refs == 0 && fileMutationQueues.queues[key] == queue {
		delete(fileMutationQueues.queues, key)
	}
}

func mutationQueueKey(path string) (string, error) {
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(resolvedPath)
	if err != nil {
		if isMissingMutationQueuePathError(err) {
			return resolvedPath, nil
		}
		return "", err
	}
	return resolved, nil
}

func isMissingMutationQueuePathError(err error) bool {
	if os.IsNotExist(err) {
		return true
	}
	if err.Error() == "not a directory" {
		return true
	}

	var pathErr *os.PathError
	return errors.As(err, &pathErr) && errors.Is(pathErr.Err, syscall.ENOTDIR)
}
