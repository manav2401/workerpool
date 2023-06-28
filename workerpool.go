package workerpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/deque"
)

const (
	// If workes idle for at least this period of time, then stop a worker.
	idleTimeout = 2 * time.Second
	NoTimeout   = 0
)

// New creates and starts a pool of worker goroutines.
//
// The maxWorkers parameter specifies the maximum number of workers that can
// execute tasks concurrently. When there are no incoming tasks, workers are
// gradually stopped until there are no remaining workers.
func New(maxWorkers int) *WorkerPool {
	// There must be at least one worker.
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	pool := &WorkerPool{
		maxWorkers:  maxWorkers,
		taskQueue:   make(chan poolTask),
		workerQueue: make(chan poolTask),
		stopSignal:  make(chan struct{}),
		stoppedChan: make(chan struct{}),
	}

	// Start the task dispatcher.
	go pool.dispatch()

	return pool
}

// WorkerPool is a collection of goroutines, where the number of concurrent
// goroutines processing requests does not exceed the specified maximum.
type WorkerPool struct {
	maxWorkers   int
	workerCount  atomic.Int64
	taskQueue    chan poolTask
	workerQueue  chan poolTask
	stoppedChan  chan struct{}
	stopSignal   chan struct{}
	waitingQueue deque.Deque[poolTask]
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	waiting      int32
	wait         bool
}

// Size returns the maximum number of concurrent workers.
func (p *WorkerPool) Size() int {
	return p.maxWorkers
}

// Stop stops the worker pool and waits for only currently running tasks to
// complete. Pending tasks that are not currently running are abandoned. Tasks
// must not be submitted to the worker pool after calling stop.
//
// Since creating the worker pool starts at least one goroutine, for the
// dispatcher, Stop() or StopWait() should be called when the worker pool is no
// longer needed.
func (p *WorkerPool) Stop() {
	p.stop(false)
}

// StopWait stops the worker pool and waits for all queued tasks tasks to
// complete. No additional tasks may be submitted, but all pending tasks are
// executed by workers before this function returns.
func (p *WorkerPool) StopWait() {
	p.stop(true)
}

// Stopped returns true if this worker pool has been stopped.
func (p *WorkerPool) Stopped() bool {
	p.stopLock.Lock()
	defer p.stopLock.Unlock()
	return p.stopped
}

// Submit enqueues a function for a worker to execute.
//
// Any external values needed by the task function must be captured in a
// closure. Any return values should be returned over a channel that is
// captured in the task function closure.
//
// Submit will not block regardless of the number of tasks submitted. Each task
// is immediately given to an available worker or to a newly started worker. If
// there are no available workers, and the maximum number of workers are
// already created, then the task is put onto a waiting queue.
//
// When there are tasks on the waiting queue, any additional new tasks are put
// on the waiting queue. Tasks are removed from the waiting queue as workers
// become available.
//
// As long as no new tasks arrive, one available worker is shutdown each time
// period until there are no more idle workers. Since the time to start new
// goroutines is not significant, there is no need to retain idle workers
// indefinitely.
func (p *WorkerPool) Submit(ctx context.Context, fn func() error, timeout time.Duration) <-chan error {
	if fn != nil {
		t := NewTask(ctx, fn, timeout)
		p.taskQueue <- t
		return t.Err
	}

	errCh := make(chan error)
	close(errCh)

	return errCh
}

// SubmitWait enqueues the given function and waits for it to be executed.
func (p *WorkerPool) SubmitWait(ctx context.Context, fn func() error, timeout time.Duration) error {
	if fn == nil {
		return nil
	}
	t := NewTask(ctx, fn, timeout)
	p.taskQueue <- t
	return <-t.Err
}

// WaitingQueueSize returns the count of tasks in the waiting queue.
func (p *WorkerPool) WaitingQueueSize() int {
	return int(atomic.LoadInt32(&p.waiting))
}

// GetWorkerCount returns the number of workers serving requests currently.
func (p *WorkerPool) GetWorkerCount() int64 {
	return p.workerCount.Load()
}

// Pause causes all workers to wait on the given Context, thereby making them
// unavailable to run tasks. Pause returns when all workers are waiting. Tasks
// can continue to be queued to the workerpool, but are not executed until the
// Context is canceled or times out.
//
// Calling Pause when the worker pool is already paused causes Pause to wait
// until all previous pauses are canceled. This allows a goroutine to take
// control of pausing and unpausing the pool as soon as other goroutines have
// unpaused it.
//
// When the workerpool is stopped, workers are unpaused and queued tasks are
// executed during StopWait.
func (p *WorkerPool) Pause(ctx context.Context) {
	p.stopLock.Lock()
	defer p.stopLock.Unlock()

	if p.stopped {
		return
	}

	ready := new(sync.WaitGroup)
	ready.Add(p.maxWorkers)

	innerCtx := context.Background()

	for i := 0; i < p.maxWorkers; i++ {
		p.Submit(innerCtx, func() error {
			ready.Done()
			select {
			case <-ctx.Done():
			case <-p.stopSignal:
			}
			return nil
		}, NoTimeout)
	}
	// Wait for workers to all be paused
	ready.Wait()
}

// dispatch sends the next queued task to an available worker.
func (p *WorkerPool) dispatch() {
	defer close(p.stoppedChan)
	timeout := time.NewTimer(idleTimeout)
	var (
		idle bool
		wg   sync.WaitGroup
	)
	p.workerCount.Store(0)

Loop:
	for {
		// As long as tasks are in the waiting queue, incoming tasks are put
		// into the waiting queue and tasks to run are taken from the waiting
		// queue. Once the waiting queue is empty, then go back to submitting
		// incoming tasks directly to available workers.
		if p.waitingQueue.Len() != 0 {
			if !p.processWaitingQueue() {
				break Loop
			}
			continue
		}

		select {
		case task, ok := <-p.taskQueue:
			if !ok {
				break Loop
			}
			// Got a task to do.
			select {
			case p.workerQueue <- task:
			default:
				// Create a new worker, if not at max.
				if int(p.workerCount.Load()) < p.maxWorkers {
					wg.Add(1)
					go worker(task, p.workerQueue, &wg)
					p.workerCount.Add(1)
					fmt.Println("*** incrementing worker count:", p.workerCount.Load())
				} else {
					// Enqueue task to be executed by next available worker.
					p.waitingQueue.PushBack(task)
					atomic.StoreInt32(&p.waiting, int32(p.waitingQueue.Len()))
				}
			}
			idle = false
		case <-timeout.C:
			// Timed out waiting for work to arrive. Kill a ready worker if
			// pool has been idle for a whole timeout.
			if idle && int(p.workerCount.Load()) > 0 {
				if p.killIdleWorker() {
					p.workerCount.Add(-1)
				}
			}
			idle = true
			timeout.Reset(idleTimeout)
		}
	}

	// If instructed to wait, then run tasks that are already queued.
	if p.wait {
		p.runQueuedTasks()
	}

	// Stop all remaining workers as they become ready.
	for int(p.workerCount.Load()) > 0 {
		p.workerQueue <- NewTask(nil, nil)
		p.workerCount.Add(-1)
	}
	wg.Wait()

	timeout.Stop()
}

type poolTask struct {
	task   func() error
	Err    chan error
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func NewTask(ctx context.Context, fn func() error, timeout ...time.Duration) poolTask {
	var (
		cancel func()
		done   chan struct{}
	)

	if len(timeout) > 0 && timeout[0] > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout[0])
		done = make(chan struct{})
	}

	return poolTask{
		task:   fn,
		Err:    make(chan error, 1),
		done:   done,
		ctx:    ctx,
		cancel: cancel,
	}
}

var ErrTimeout = errors.New("timeout")

// worker executes tasks and stops when it receives a nil task.
func worker(task poolTask, workerQueue chan poolTask, wg *sync.WaitGroup) {
	errCh := make(chan error, 1)
	tasks := make(chan poolTask, 1)

	defer close(tasks)

	go func() {
		for execTask := range tasks {
			err := execTask.task()

			select {
			case <-execTask.ctx.Done():
			case execTask.Err <- err:
			default:
			}

			close(execTask.done)
		}
	}()

	for task.task != nil {
		if task.cancel == nil {
			task.Err <- task.task()
			task = <-workerQueue
		} else {
			tasks <- task

			var err error

			select {
			case <-task.done:
				err = <-errCh
			case <-task.ctx.Done():
				err = ErrTimeout
			}

			task.Err <- err
			task.cancel()
			task = <-workerQueue
		}
	}
	wg.Done()
}

// stop tells the dispatcher to exit, and whether or not to complete queued
// tasks.
func (p *WorkerPool) stop(wait bool) {
	p.stopOnce.Do(func() {
		// Signal that workerpool is stopping, to unpause any paused workers.
		close(p.stopSignal)
		// Acquire stopLock to wait for any pause in progress to complete. All
		// in-progress pauses will complete because the stopSignal unpauses the
		// workers.
		p.stopLock.Lock()
		// The stopped flag prevents any additional paused workers. This makes
		// it safe to close the taskQueue.
		p.stopped = true
		p.stopLock.Unlock()
		p.wait = wait
		// Close task queue and wait for currently running tasks to finish.
		close(p.taskQueue)
	})
	<-p.stoppedChan
}

// processWaitingQueue puts new tasks onto the the waiting queue, and removes
// tasks from the waiting queue as workers become available. Returns false if
// worker pool is stopped.
func (p *WorkerPool) processWaitingQueue() bool {
	select {
	case task, ok := <-p.taskQueue:
		if !ok {
			return false
		}
		p.waitingQueue.PushBack(task)
	case p.workerQueue <- p.waitingQueue.Front():
		// A worker was ready, so gave task to worker.
		p.waitingQueue.PopFront()
	}
	atomic.StoreInt32(&p.waiting, int32(p.waitingQueue.Len()))
	return true
}

func (p *WorkerPool) killIdleWorker() bool {
	select {
	case p.workerQueue <- NewTask(nil, nil):
		// Sent kill signal to worker.
		return true
	default:
		// No ready workers. All, if any, workers are busy.
		return false
	}
}

// runQueuedTasks removes each task from the waiting queue and gives it to
// workers until queue is empty.
func (p *WorkerPool) runQueuedTasks() {
	for p.waitingQueue.Len() != 0 {
		// A worker is ready, so give task to worker.
		p.workerQueue <- p.waitingQueue.PopFront()
		atomic.StoreInt32(&p.waiting, int32(p.waitingQueue.Len()))
	}
}
