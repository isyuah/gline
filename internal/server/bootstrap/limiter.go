package bootstrap

import (
	"context"
	"sync"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/server/query"
)

type projectLimiter struct {
	mu       sync.Mutex
	capacity int
	projects map[domain.ProjectID]*projectSemaphore
}

type projectSemaphore struct {
	tokens chan struct{}
	users  int
}

func newProjectLimiter(capacity int) *projectLimiter {
	return &projectLimiter{capacity: capacity, projects: make(map[domain.ProjectID]*projectSemaphore)}
}

func (l *projectLimiter) Acquire(ctx context.Context, projectID domain.ProjectID) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	semaphore := l.projects[projectID]
	if semaphore == nil {
		semaphore = &projectSemaphore{tokens: make(chan struct{}, l.capacity)}
		l.projects[projectID] = semaphore
	}
	semaphore.users++
	l.mu.Unlock()

	select {
	case semaphore.tokens <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-semaphore.tokens
				l.releaseUser(projectID, semaphore)
			})
		}, nil
	case <-ctx.Done():
		l.releaseUser(projectID, semaphore)
		return nil, ctx.Err()
	default:
		l.releaseUser(projectID, semaphore)
		return nil, query.ErrCapacityLimited
	}
}

func (l *projectLimiter) releaseUser(projectID domain.ProjectID, semaphore *projectSemaphore) {
	l.mu.Lock()
	defer l.mu.Unlock()
	semaphore.users--
	if semaphore.users == 0 && len(semaphore.tokens) == 0 {
		delete(l.projects, projectID)
	}
}
