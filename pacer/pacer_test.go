package pacer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/JekaMas/workerpool"
)

func TestPacedWorkers(t *testing.T) {
	t.Parallel()

	delay1 := 100 * time.Millisecond
	delay2 := 300 * time.Millisecond
	wp := workerpool.New(5)
	defer wp.Stop()

	pacer := NewPacer(delay1)
	defer pacer.Stop()

	slowPacer := NewPacer(delay2)
	defer slowPacer.Stop()

	tasksDone := new(sync.WaitGroup)
	tasksDone.Add(20)
	start := time.Now()

	pacedTask := pacer.Pace(func() error {
		//fmt.Println("Task")
		tasksDone.Done()
		return nil
	})

	slowPacedTask := slowPacer.Pace(func() error {
		//fmt.Println("SlowTask")
		tasksDone.Done()
		return nil
	})

	// Cause worker to be created, and available for reuse before next task.
	for i := 0; i < 10; i++ {
		wp.Submit(context.Background(), pacedTask, workerpool.NoTimeout)
		wp.Submit(context.Background(), slowPacedTask, workerpool.NoTimeout)
	}

	time.Sleep(500 * time.Millisecond)
	pacer.Pause()
	time.Sleep(time.Second)
	if !pacer.IsPaused() {
		t.Fatal("should be paused")
	}
	pacer.Resume()
	if pacer.IsPaused() {
		t.Fatal("should not be paused")
	}

	tasksDone.Wait()
	elapsed := time.Since(start)
	// 9 times delay2 since no wait for first task, and pacer and slowPacer run
	// currently so only limiter is slowPacer.
	if elapsed < 9*delay2 {
		t.Fatal("Did not pace tasks correctly - finished too soon:", elapsed)
	}
}
