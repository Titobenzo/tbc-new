package hunter

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/wowsims/tbc/sim/core"
	"github.com/wowsims/tbc/sim/core/proto"
	"github.com/wowsims/tbc/sim/core/simsignals"
	googleProto "google.golang.org/protobuf/proto"
)

func hunterScalingRequest(t *testing.T) *proto.RaidSimRequest {
	rotation := core.GetAplRotation("../../ui/hunter/dps/apls", "default")
	gens := core.FullCharacterTestSuiteGenerator([]core.CharacterSuiteConfig{
		{
			Class:            proto.Class_ClassHunter,
			Race:             proto.Race_RaceOrc,
			GearSet:          core.GetGearSet("../../ui/hunter/dps/gear_sets/phase_2/bm", "2h_6p"),
			Talents:          DefaultBMTalents,
			Consumables:      DefaultConsumables,
			SpecOptions:      core.SpecOptionsCombo{Label: "Default", SpecOptions: DefaultOptions},
			StartingDistance: 7,
			Rotation:         rotation,
		},
	})
	for _, gen := range gens {
		for i := 0; i < gen.NumTests(); i++ {
			if _, _, _, rsr := gen.GetTest(i); rsr != nil {
				return rsr
			}
		}
	}
	t.Fatal("could not build a hunter RaidSimRequest")
	return nil
}

// TestHunterParallelScaling (RUN_SCALING=1) measures how independent hunter sims scale across cores.
// Established that the engine scales to ~physical-core count in isolation regardless of iteration
// count, fight length, varied gear, shared vs fresh signals, GC setting, channel dispatch, or a
// large live heap - so the batch's failure to scale is specific to the running server, not the sim.
func TestHunterParallelScaling(t *testing.T) {
	if os.Getenv("RUN_SCALING") == "" {
		t.Skip("perf probe; set RUN_SCALING=1 to run")
	}
	rsr := hunterScalingRequest(t)
	rsr.SimOptions.Iterations = 100
	rsr.SimOptions.IsTest = false
	rsr.SimOptions.RandomSeed = 1
	rsr.Encounter.Duration = 20
	rsr.Encounter.DurationVariation = 0

	if res := core.RunSim(googleProto.Clone(rsr).(*proto.RaidSimRequest), nil, simsignals.CreateSignals()); res.Error != nil {
		t.Fatalf("sample sim errored: %s", res.Error.Message)
	}

	run := func(workers, perWorker int) float64 {
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				clone := googleProto.Clone(rsr).(*proto.RaidSimRequest)
				for j := 0; j < perWorker; j++ {
					core.RunSim(clone, nil, simsignals.CreateSignals())
				}
			}()
		}
		wg.Wait()
		return float64(workers*perWorker) / time.Since(start).Seconds()
	}

	n := runtime.NumCPU()
	run(1, 4) // warm
	seq := run(1, 24)
	par := run(n, 24)
	t.Logf("1 core = %6.1f sims/s | %d cores = %6.1f sims/s | speedup = %4.1fx", seq, n, par, par/seq)
}
