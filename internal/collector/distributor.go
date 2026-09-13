package collector

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/queue"
)

// WorkDistributor manages intelligent work distribution across workers.
type WorkDistributor struct {
	queue queue.Client
	mu    sync.RWMutex
}

// NewWorkDistributor creates a new work distributor.
func NewWorkDistributor(queue queue.Client) *WorkDistributor {
	return &WorkDistributor{queue: queue}
}

// WorkerLoad represents the load on a worker.
type WorkerLoad struct {
	WorkerID     string
	WorkerType   string
	AssignedPods int
	Healthy      bool
}

// DistributeWork selects the optimal worker for a task.
func (wd *WorkDistributor) DistributeWork(ctx context.Context, podKey, workType string) (string, error) {
	wd.mu.Lock()
	defer wd.mu.Unlock()

	workers, err := wd.queue.GetAllWorkerHealth(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get worker health: %w", err)
	}

	available := wd.filterWorkers(workers, workType)
	if len(available) == 0 {
		return "", fmt.Errorf("no healthy workers available for type %s", workType)
	}

	selected := wd.selectWorker(available, podKey)

	logger.Logger.Debug("Work distributed",
		zap.String("pod", podKey),
		zap.String("type", workType),
		zap.String("worker", selected.WorkerID),
		zap.Int("load", selected.AssignedPods))

	return selected.WorkerID, nil
}

func (wd *WorkDistributor) filterWorkers(workers map[string]*queue.WorkerHealth, workType string) []WorkerLoad {
	var available []WorkerLoad

	for _, worker := range workers {
		if worker.Type == workType && worker.Healthy {
			available = append(available, WorkerLoad{
				WorkerID:     worker.WorkerID,
				WorkerType:   worker.Type,
				AssignedPods: len(worker.AssignedPods),
				Healthy:      worker.Healthy,
			})
		}
	}

	return available
}

func (wd *WorkDistributor) selectWorker(workers []WorkerLoad, podKey string) WorkerLoad {
	if len(workers) == 1 {
		return workers[0]
	}

	// Sort by load, then by ID for consistency
	sort.Slice(workers, func(i, j int) bool {
		if workers[i].AssignedPods == workers[j].AssignedPods {
			return workers[i].WorkerID < workers[j].WorkerID
		}
		return workers[i].AssignedPods < workers[j].AssignedPods
	})

	minLoad := workers[0].AssignedPods

	// Get workers with minimum load
	var candidates []WorkerLoad
	for _, worker := range workers {
		if worker.AssignedPods == minLoad {
			candidates = append(candidates, worker)
		} else {
			break
		}
	}

	if len(candidates) == 1 {
		return candidates[0]
	}

	// Use consistent hashing for selection
	hasher := fnv.New32a()
	hasher.Write([]byte(podKey))
	hash := hasher.Sum32()

	return candidates[int(hash)%len(candidates)]
}

func (wd *WorkDistributor) GetWorkerDistribution(ctx context.Context) (map[string]WorkerLoad, error) {
	wd.mu.RLock()
	defer wd.mu.RUnlock()

	workers, err := wd.queue.GetAllWorkerHealth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get worker health: %w", err)
	}

	distribution := make(map[string]WorkerLoad)
	for id, worker := range workers {
		distribution[id] = WorkerLoad{
			WorkerID:     worker.WorkerID,
			WorkerType:   worker.Type,
			AssignedPods: len(worker.AssignedPods),
			Healthy:      worker.Healthy,
		}
	}

	return distribution, nil
}
