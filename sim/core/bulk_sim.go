package core

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/wowsims/tbc/sim/core/proto"
	"github.com/wowsims/tbc/sim/core/simsignals"
)

// RunBulkSimConcurrentAsync runs many independent raid sim requests (e.g. a bulk gear
// comparison) in a single call, distributing whole sims across CPU cores - one combination
// per core rather than splitting each combination across all cores. This avoids the
// per-combination network round-trip and serialization overhead of running every
// combination as its own request, which dominates when each sim is tiny.
func RunBulkSimConcurrentAsync(request *proto.BulkSimRequest, progress chan *proto.ProgressMetrics, requestId string) {
	signals, err := simsignals.RegisterWithId(requestId)
	if err != nil {
		progress <- &proto.ProgressMetrics{
			FinalBulkResult: &proto.BulkSimResult{
				Results: []*proto.RaidSimResult{{
					Error: &proto.ErrorOutcome{Message: "Couldn't register for signal API: " + err.Error()},
				}},
			},
		}
		return
	}
	go func() {
		defer simsignals.UnregisterId(requestId)
		runBulkSim(request, progress, signals)
	}()
}

func runBulkSim(request *proto.BulkSimRequest, progress chan *proto.ProgressMetrics, signals simsignals.Signals) {
	requests := request.Requests
	total := int32(len(requests))
	results := make([]*proto.RaidSimResult, len(requests))

	var completed atomic.Int32

	// Report progress without ever blocking a worker: drop the update if the consumer is
	// behind, since only the latest matters for the progress bar.
	reportProgress := func() {
		if progress == nil {
			return
		}
		select {
		case progress <- &proto.ProgressMetrics{CompletedSims: completed.Load(), TotalSims: total}:
		default:
		}
	}

	// Run one whole sim on a single goroutine, recovering panics into an error result so a
	// single bad combination cannot take down the entire batch.
	runOne := func(i int) {
		defer func() {
			if err := recover(); err != nil {
				results[i] = &proto.RaidSimResult{
					Error: &proto.ErrorOutcome{Message: fmt.Sprintf("%v\nStack Trace:\n%s", err, debug.Stack())},
				}
			}
			completed.Add(1)
			reportProgress()
		}()
		results[i] = RunSim(requests[i], nil, signals)
	}

	numWorkers := runtime.NumCPU()
	if numWorkers > len(requests) {
		numWorkers = len(requests)
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	indexCh := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indexCh {
				if signals.Abort.IsTriggered() {
					continue // drain remaining indices without running; aborted combos stay nil for now
				}
				runOne(i)
			}
		}()
	}

	for i := range requests {
		indexCh <- i
	}
	close(indexCh)
	wg.Wait()

	// Repeated message fields can't carry nils across the wire; fill any combination that
	// never ran (e.g. because of an abort) with an explicit aborted result.
	for i := range results {
		if results[i] == nil {
			results[i] = &proto.RaidSimResult{Error: &proto.ErrorOutcome{Type: proto.ErrorOutcomeType_ErrorOutcomeAborted}}
		}
	}

	if progress != nil {
		// The final result must arrive, so this send blocks (unlike progress updates above).
		progress <- &proto.ProgressMetrics{
			CompletedSims:   completed.Load(),
			TotalSims:       total,
			FinalBulkResult: &proto.BulkSimResult{Results: results},
		}
	}
}
