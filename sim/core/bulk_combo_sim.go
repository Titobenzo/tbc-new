package core

import (
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wowsims/tbc/sim/core/proto"
	"github.com/wowsims/tbc/sim/core/simsignals"
	"github.com/wowsims/tbc/sim/core/stats"
	googleProto "google.golang.org/protobuf/proto"
)


// bulkComboWorkers is how many combinations to sim in parallel. Defaults to all logical CPUs, but
// these sims are memory/cache-heavy, so on a hyperthreaded machine fewer workers (≈ physical core
// count) often run FASTER - each fight keeps a core's cache to itself instead of thrashing it with
// its hyperthread sibling. Override with BULK_WORKERS to tune without rebuilding.
func bulkComboWorkers(total int) int {
	n := runtime.NumCPU()
	if env := os.Getenv("BULK_WORKERS"); env != "" {
		if parsed, err := strconv.Atoi(env); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > total {
		n = total
	}
	if n < 1 {
		n = 1
	}
	return n
}

const bulkComboTopN = 5

// RunBulkComboSimConcurrentAsync expands the combination space (sent as dimensions by the
// frontend), sims every combination across all CPU cores - one whole combination per core -
// keeps the best few, and returns them ranked alongside the baseline (current) gear. All of the
// heavy work (the cartesian product, applying gear, simming) runs natively here; the browser only
// sends the option lists and renders the ranked table.
func RunBulkComboSimConcurrentAsync(request *proto.BulkComboSimRequest, progress chan *proto.ProgressMetrics, requestId string) {
	signals, err := simsignals.RegisterWithId(requestId)
	if err != nil {
		progress <- &proto.ProgressMetrics{FinalComboResult: &proto.BulkComboSimResult{}}
		return
	}
	go func() {
		defer simsignals.UnregisterId(requestId)
		runBulkComboSim(request, progress, signals)
	}()
}

type rankedCombo struct {
	equipment *proto.EquipmentSpec
	dps       *proto.DistributionMetrics
}

// trimDist drops the per-iteration history/values from a distribution; the ranked table only
// needs the summary stats (avg/stdev/min/max), and this keeps the returned payload small.
func trimDist(d *proto.DistributionMetrics) *proto.DistributionMetrics {
	if d == nil {
		return nil
	}
	d.Hist = nil
	d.AllValues = nil
	return d
}

func runBulkComboSim(request *proto.BulkComboSimRequest, progress chan *proto.ProgressMetrics, signals simsignals.Signals) {
	dims := dimensionsToChoices(request.Dimensions)
	total := int32(comboCount(dims))

	basePlayer := func(req *proto.RaidSimRequest) *proto.Player {
		if req.Raid == nil || len(req.Raid.Parties) == 0 || len(req.Raid.Parties[0].Players) == 0 {
			return nil
		}
		return req.Raid.Parties[0].Players[0]
	}
	if basePlayer(request.Base) == nil {
		if progress != nil {
			progress <- &proto.ProgressMetrics{FinalComboResult: &proto.BulkComboSimResult{TotalCombinations: total}}
		}
		return
	}

	// Baseline: sim the current gear as-is. Run it first, single-threaded, so it warms the global
	// item database (request.Base's player database must contain every bulk item) before the
	// concurrent combo sims below, which then only read it.
	baseResult := RunSim(request.Base, nil, signals)
	baseline := &proto.RankedGearResult{
		Equipment: basePlayer(request.Base).Equipment,
		Dps:       trimDist(baseResult.RaidMetrics.Dps),
	}

	var completed atomic.Int32

	var mu sync.Mutex
	top := make([]rankedCombo, 0, bulkComboTopN+1)
	// insertTop considers a finished combo for the top-N. equip is cloned (and dps trimmed) only if
	// the combo actually makes the cut, so the vast majority of combos cost no allocation.
	insertTop := func(equip *proto.EquipmentSpec, dps *proto.DistributionMetrics) {
		mu.Lock()
		defer mu.Unlock()
		if len(top) >= bulkComboTopN && dps.Avg <= top[len(top)-1].dps.Avg {
			return
		}
		top = append(top, rankedCombo{equipment: googleProto.Clone(equip).(*proto.EquipmentSpec), dps: trimDist(dps)})
		sort.Slice(top, func(i, j int) bool { return top[i].dps.Avg > top[j].dps.Avg })
		if len(top) > bulkComboTopN {
			top = top[:bulkComboTopN]
		}
	}

	numWorkers := bulkComboWorkers(int(total))

	// These parallel sims churn a lot of garbage. GC backoff during the run (restored after) keeps
	// cycles on simming rather than collecting. Tunable via BULK_GOGC ("off"/-1 disables GC entirely;
	// a number sets the GOGC percent) so we can tell GC mark-assist apart from raw allocation cost.
	gogc := 400
	if env := os.Getenv("BULK_GOGC"); env != "" {
		if env == "off" {
			gogc = -1
		} else if parsed, err := strconv.Atoi(env); err == nil {
			gogc = parsed
		}
	}
	prevGC := debug.SetGCPercent(gogc)
	defer debug.SetGCPercent(prevGC)

	// Gem optimization setup (Suggest Gems, ported). When enabled, each combo is re-gemmed to
	// maximize EP under the caps before simming. The candidate pool is built once here.
	optimizeGems := request.Settings != nil && request.Settings.OptimizeGems
	var gemPool []gemOption
	var gemEP stats.Stats
	var gemCapsConfig []*proto.GemStatCap
	var metaGemId int32
	var metaCond metaCondition
	if optimizeGems {
		gemEP = stats.FromProtoArray(request.Settings.GemEpWeights)
		gemCapsConfig = request.Settings.GemCaps
		gemPool = buildGemPool(request.Settings.GemPoolIds, request.Settings.DisableUniqueGems)
		metaCond = metaConditionFromSettings(request.Settings)
		// Effective meta gem: the configured fallback, or (if unset) the one the player already has
		// equipped, so the meta carries forward onto whichever helmet a combo uses.
		metaGemId = request.Settings.DefaultMetaGem
		if metaGemId == 0 {
			metaGemId = equippedMetaGem(basePlayer(request.Base).Equipment)
		}
	}

	// Diagnostic: BULK_PROFILE=1 turns on mutex+block profiling so /debug/pprof/{mutex,block} reveal
	// any lock/channel contention in the live server (a CPU profile only shows busy cores, not waits).
	if os.Getenv("BULK_PROFILE") != "" {
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(10000)
		defer runtime.SetMutexProfileFraction(0)
		defer runtime.SetBlockProfileRate(0)
	}

	// Report progress on a timer, not per-combo: per-combo sends woke the HTTP long-poll handler
	// thousands of times/sec, and that scheduler churn migrates workers across Ps and thrashes the
	// per-P allocator caches (forcing contended mcentral refills). The ticker decouples them.
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		lastLog := time.Now()
		lastDone := int32(0)
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				done := completed.Load()
				if progress != nil {
					select {
					case progress <- &proto.ProgressMetrics{CompletedSims: done, TotalSims: total}:
					default:
					}
				}
				// Log the instantaneous rate every 5s so we can see if it decays during the run.
				if since := time.Since(lastLog); since > 5*time.Second {
					log.Printf("[bulkcombo] progress: %d/%d done, %.0f combos/sec (last %s)",
						done, total, float64(done-lastDone)/since.Seconds(), since.Round(time.Second))
					lastLog = time.Now()
					lastDone = done
				}
			}
		}
	}()

	startTime := time.Now()
	indexCh := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Clone the (heavy) base request once per worker and rewrite only the varied slots per
			// combination, instead of cloning per combination.
			baseClone := googleProto.Clone(request.Base).(*proto.RaidSimRequest)
			player := baseClone.Raid.Parties[0].Players[0]
			for i := range indexCh {
				if signals.Abort.IsTriggered() {
					completed.Add(1)
					continue
				}
				func() {
					defer func() {
						_ = recover() // a single bad combination must not sink the whole batch
						completed.Add(1)
					}()
					applyComboToEquipment(player.Equipment, comboAt(dims, i))
					if optimizeGems {
						// Keep/fill meta gems and strip the rest, so the no-gem baseline (for caps)
						// includes the meta gem and the meta socket isn't left empty, then compute
						// the non-gem character stats, optimize the non-meta sockets, and sim.
						prepareMetaGems(player.Equipment, metaGemId)
						statsRes := ComputeStats(&proto.ComputeStatsRequest{Raid: baseClone.Raid, Encounter: baseClone.Encounter})
						if p := firstPlayerStats(statsRes); p != nil {
							sNonGem := stats.FromUnitStatsProto(p.FinalStats)
							// Solve the same LP as Suggest Gems (caps + unique + meta-color condition +
							// socket bonuses). Fall back to the greedy optimizer if the LP can't produce a
							// complete solution (e.g. an infeasible meta condition for this gear).
							if !optimizeEquipmentGemsLP(player.Equipment, sNonGem, gemEP, p.FinalStats.PseudoStats, gemCapsConfig, gemPool, metaCond) {
								curves := buildGemCurves(gemCapsConfig, sNonGem, p.FinalStats.PseudoStats, gemEP)
								optimizeEquipmentGems(player.Equipment, sNonGem, gemEP, curves, gemPool)
							}
						}
					}
					// Build the sim, then run it. Presim is skipped (NewSim+run doesn't presim): it's a
					// fixed ~100-iteration pass that would dominate at low iteration counts.
					sim := NewSim(baseClone, signals)
					sim.dpsOnly = true // ranking needs only raid DPS; skip the full metrics tree
					result := sim.run()
					if result.Error != nil {
						return
					}
					// Snapshot the equipment - the worker overwrites player.Equipment next iteration.
					insertTop(player.Equipment, result.RaidMetrics.Dps)
				}()
			}
		}()
	}

	for i := 0; i < int(total); i++ {
		if signals.Abort.IsTriggered() {
			break
		}
		indexCh <- i
	}
	close(indexCh)
	wg.Wait()
	close(progressDone)

	iters := int32(0)
	if request.Base.SimOptions != nil {
		iters = request.Base.SimOptions.Iterations
	}
	elapsed := time.Since(startTime)
	done := completed.Load() // ACTUAL combos finished - less than total if the run was cancelled
	status := "done"
	if done < total {
		status = "CANCELLED"
	}
	rate := 0.0
	if elapsed.Seconds() > 0 {
		rate = float64(done) / elapsed.Seconds()
	}
	eta := time.Duration(0)
	if rate > 0 {
		eta = time.Duration(float64(total)/rate) * time.Second
	}
	log.Printf("[bulkcombo] %s: %d/%d combos @ %d iters across %d workers in %s (%.1f combos/sec, full run ETA %s)",
		status, done, total, iters, numWorkers, elapsed.Round(time.Millisecond), rate, eta.Round(time.Second))

	ranked := make([]*proto.RankedGearResult, len(top))
	for i, t := range top {
		ranked[i] = &proto.RankedGearResult{Equipment: t.equipment, Dps: t.dps}
	}

	if progress != nil {
		progress <- &proto.ProgressMetrics{
			CompletedSims: completed.Load(),
			TotalSims:     total,
			FinalComboResult: &proto.BulkComboSimResult{
				Ranked:            ranked,
				Baseline:          baseline,
				TotalCombinations: total,
			},
		}
	}
}
