// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import "sync"

// pool bounds the number of api.*/db.* calls in flight at once. Submission
// never blocks the caller (the event loop must never wait on the pool): each
// task is its own goroutine parked on the semaphore until a slot frees up, so
// pending work queues in scheduler space rather than stalling JavaScript.
type pool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func newPool(size int) *pool {
	return &pool{sem: make(chan struct{}, size)}
}

// submit schedules task to run when a slot is available.
func (p *pool) submit(task func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.sem <- struct{}{}
		defer func() { <-p.sem }()
		task()
	}()
}

// wait blocks until every submitted task has finished. Used by Runtime.Close
// to drain in-flight calls before teardown.
func (p *pool) wait() { p.wg.Wait() }
