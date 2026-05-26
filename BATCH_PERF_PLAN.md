# Batch sim performance plan — CRN + adaptive iteration allocation

**Audience:** a fresh coding agent picking this up cold, in a later session, with no prior context.
**Goal:** make the native bulk-gear "batch" sim dramatically faster *without* losing ranking correctness, by spending simulation iterations only where they're needed instead of running every gear combination to full precision.

Read this whole document before writing code. The **measurement phase (Phase 0) is mandatory and comes first** — the entire premise is currently unquantified, and the failure mode (reporting the wrong "best gear") is silent. Do not skip to the optimization phases.

---

## 0. Orientation (do this first)

- **Repo:** `~/Downloads/tbc-tracked` — a *private feature fork* of the open-source wowsims TBC simulator. Branch **`batch-improvements`**.
- **What it is:** a WoW TBC Classic DPS sim. Go engine (`sim/`) compiles to WASM (browser) and to a native server binary (`wowsimtbc`). TypeScript UI (`ui/`). Protobuf (`proto/`) shared between them. See `CLAUDE.md` at the repo root for the general architecture.
- **The "batch" feature (what this plan optimizes):** compares many gear combinations. The frontend sends per-slot option lists ("dimensions"); the Go backend expands the cartesian product, re-gems each combo (an LP optimizer), sims each, and returns the top-N ranked sets. All the heavy work is server-side.

### Build / run / test loop (non-obvious — there are traps)
- **Build the server (required toolchain):** `GOTOOLCHAIN=go1.25.4 GOEXPERIMENT= make wowsimtbc`
  Plain `make wowsimtbc` FAILS — this machine's default Go (1.26.x) crashes the WASM build with a stdlib `ctrlEmpty redeclared` bug. Always use the `GOTOOLCHAIN=go1.25.4` prefix (also for `make proto`).
- **Backend tests:** `GOTOOLCHAIN=go1.25.4 GOARCH=amd64 go test --tags=with_db ./sim/...` (the `with_db` tag loads the item DB; required).
- **Golden-file tests:** spec tests compare against committed `.results` files. If a change legitimately alters sim output, `make update-tests` promotes the new results — but for THIS work, changing golden DPS is a red flag (see invariants).
- **Frontend type-check:** `npx tsc --noEmit`. Regenerate proto after `.proto` edits: `GOTOOLCHAIN=go1.25.4 GOEXPERIMENT= make proto` (generated `*.pb.go` / `ui/core/proto/*.ts` are gitignored).
- **Run loop trap:** the binary EMBEDS the client. A UI/TS change needs a full rebuild **and a server restart** — a running `./wowsimtbc` holds the old client in memory. Launch `./wowsimtbc hunter`; it serves at `http://localhost:3333/tbc/hunter/` (redirects to `/dps/`). Kill the old process (find the pid on `:3333`) before relaunching.
- **Commit discipline:** commit only when the user explicitly asks.

### Key source files for this work
- `sim/core/bulk_combo_sim.go` — **the batch runner** (`runBulkComboSim`). Worker pool, per-combo loop (`applyComboToEquipment` → re-gem → `NewSim` → `sim.run()` → keep top-N), long-poll progress. This is where adaptive allocation gets implemented.
- `sim/core/sim.go` — the core engine. **RNG seeding lives here** (critical for CRN): `newSimWithEnv` (~line 200), `reseedRands` (~line 253), `labelRand`/`RandomFloat` (~line 235-251). `Simulation.run()` / `RunOnce` is the per-iteration fight loop.
- `sim/core/bulk_combos.go` — combo enumeration + `applyComboToEquipment`.
- `sim/core/gem_lp.go` + `sim/core/lp/` — the per-combo gem optimizer (an LP). Not the focus here, but it's per-combo fixed overhead, so adaptive allocation interacts with it (see Phase 3 note on caching).
- `proto/api.proto` — `SimOptions.random_seed` (field 2), `SimOptions.use_labeled_rands`, `BulkSettings`, `BulkComboSimRequest`.
- `ui/core/components/individual_sim_ui/bulk_tab.tsx` — the batch UI; builds the request, including iteration count and the base `RaidSimRequest` (`buildGearSimRequest`).

### Supplementary context (if available)
This machine's Claude auto-memory (`~/.claude/projects/-home-titobenzo-Downloads-tbc-new-master/memory/`) has three notes — `wowsims-batch-native-rewrite`, `wowsims-batch-correctness-bar`, `wowsims-batch-perf-ceiling` — covering the build quirks, the gem-LP, and the history of the (resolved) "doesn't scale with cores" investigation. Read them if you have access; this plan is self-contained otherwise.

---

## 1. Why this is the only lever left

The cores are already saturated and the per-iteration fight loop runs at full throughput (it's allocation-bandwidth bound, ~16k iter/sec on the user's 16 threads for these light fights). Measured throughput scales ~linearly with iteration count: ~3.4 combos/sec @5000 iters, ~32 @500, ~150 @100, ~800 @10. So:

- At high iteration counts, **>95% of each combo's time is the fight loop** — already maxed, nothing to reshuffle.
- The waste is **structural, not mechanical**: the batch sims *every* combo to full precision when it only needs to confidently identify the **top few**. Most combos are obviously worse and don't deserve full iterations.

So the win comes from **statistics, not hardware**: (A) make per-combo comparisons far less noisy (CRN), and (B) stop spending iterations on combos that are confidently out of contention (adaptive allocation). They compound.

---

## 2. The two techniques

### A. Common Random Numbers (CRN)
Each combo's DPS is noisy (crit/proc/fight-length randomness). When comparing two near-identical gear sets, independent randomness means the measured gap = real gap + (luck_A − luck_B); the luck term forces many iterations to average out. If both sets experience the **same** randomness, the luck term cancels and the measured gap is almost pure signal. Formally `Var(A−B) = Var(A) + Var(B) − 2·Cov(A,B)`; shared randomness makes `Cov` large and positive, collapsing the variance of the *difference*. Gear sets are extremely similar → high correlation → big variance reduction on exactly the quantity ranking depends on. **CRN improves comparisons, not the absolute DPS of any single set.**

### B. Adaptive iteration allocation
Rank cheaply at low iterations, then spend more iterations only on combos still in contention, eliminating the rest. The win scales with how many combos can be **confidently** dropped early — large when the field is mostly clearly-bad combos, small when it's a tight cluster of near-equal contenders. **Hard cuts at low iters are unsafe** (a good combo can look bad on an unlucky 10-iteration sample); elimination must be confidence-based. CRN is what makes the cheap early ranking trustworthy, so the two are multiplicative.

---

## 3. Engine findings that make CRN cheap (verify, then exploit)

From `sim/core/sim.go`:
- `reseedRands(i)` sets the seed to `Options.RandomSeed + i` for iteration `i`. **If every combo uses the same fixed `RandomSeed`, iteration `i` of every combo starts from the same seed** → CRN "level 0" essentially for free.
- BUT in non-test mode all `RandomFloat` calls draw from **one shared stream** (`sim.rand`). So two gear sets stay correlated only until their RNG-draw counts diverge (different proc/crit counts branch the rotation), after which the streams desync mid-fight. Correlation is strong early, decays later.
- `newSimWithEnv`: if `RandomSeed == 0` it falls back to `time.Now().UnixNano()` → **independent per combo → no CRN.** So the first thing to check is whether the batch pins a fixed non-zero seed.
- **Labeled rands** (`isTest` / `SimOptions.use_labeled_rands`): each RNG callsite `label` gets its **own** stream seeded by `makeTestRandSeed(seed, label)`. This is per-effect RNG isolation — a different proc count on one effect no longer desyncs another effect's rolls. Enabling this for batch combos gives **robust** CRN (alignment per effect, per iteration), at the cost of: (1) it's the "test" path (per-call map lookup + extra allocation → slower per iteration), and (2) it may shift absolute DPS slightly vs the production RNG path. Measure both.

---

## 4. Phased plan

Each phase has an explicit **validation gate**. Do not proceed to the next phase until the current gate passes. Keep each phase behind an env var or request flag so it can be toggled and A/B'd.

### Phase 0 — Measurement harness + correctness oracle (MANDATORY, do first)
You cannot tune what you can't measure, and the failure mode is silent. Build the instrumentation before any optimization.
1. Pick a fixed, representative batch (e.g. a real hunter gear batch of a few hundred to a few thousand combos). Save it so every later phase runs the identical input.
2. Establish the **ground-truth ranking**: run the current code at high iterations (e.g. 10k) to get a reference top-N and reference DPS spread. This is the oracle every optimization is graded against.
3. Add a measurement mode to `runBulkComboSim` (env-gated, e.g. `BULK_MEASURE=1`) that records, per combo: mean DPS, DPS variance/stdev, and iterations spent. Emit a summary (and optionally a CSV) so you can compute, after any change:
   - **Ranking accuracy:** does the produced top-N match the oracle top-N? (Kendall-tau or simple "top-K overlap" + "did the #1 change?")
   - **Cost:** total iterations executed (the real currency — wall time is noisy).
4. Define the **acceptance metric** up front: e.g. "reproduce the oracle's top-5 set (and the #1) with ≥X% confidence using ≤Y% of the iterations." Agree this with the user.

**Gate:** you can run the fixed batch, get the oracle ranking, and print ranking-accuracy + total-iterations for any run. No optimization yet.

### Phase 1 — CRN level 0 (fixed shared seed). Cheap, do it early.
1. Determine whether the batch currently pins `SimOptions.RandomSeed`. Inspect `buildGearSimRequest` in `bulk_tab.tsx` and the base request in `bulk_combo_sim.go`. If it's 0 / unset, every combo is independently time-seeded (no CRN).
2. Force a **fixed non-zero** `RandomSeed` for all batch combos (all combos share it → iteration `i` shares a seed across combos). This is a one-line-ish change but verify the seed actually reaches each `NewSim` (the base request is cloned per worker, so a value on `request.Base.SimOptions` propagates).
3. Using Phase 0's harness, measure the **variance of the DPS *difference*** between pairs of combos (e.g. each combo vs the current leader) at a fixed low iteration count, with vs without the shared seed.

**Gate:** quantify the variance-of-difference reduction. If shared-seed meaningfully tightens pairwise comparisons (expected), keep it. If the engine was already pinning the seed, document that CRN-level-0 is already in effect and move on.

### Phase 2 — CRN level 1 (labeled rands). Measure cost vs benefit.
1. Run batch combos with `UseLabeledRands` enabled and the shared fixed seed. This aligns each effect's rolls per iteration across combos (robust CRN).
2. Measure: (a) further variance-of-difference reduction vs Phase 1, and (b) the per-iteration **slowdown** from the labeled-rand path (map lookups + allocation). Also confirm whether absolute DPS shifts (it may; that's acceptable for the ranking pass but the final top-N re-sim must use the production path — see Phase 3 step 5).

**Gate:** net win = (fewer iterations needed to rank, thanks to lower variance) must outweigh (per-iteration slowdown). If labeled rands cost more than they save, stop at level 0. Decide based on numbers, not intuition.

### Phase 3 — Adaptive iteration allocation (the big structural win)
Restructure `runBulkComboSim` from "sim each combo once at fixed iters" to a **multi-round elimination**. Recommended first algorithm: **successive halving** (simple, robust, few knobs):
1. Start with all combos and a small per-combo budget `b` (e.g. 50–100 iters).
2. Sim every surviving combo for `b` more iterations, **accumulating** running mean/variance (do NOT restart each combo — keep its stats across rounds; with CRN, sim the same iteration indices across survivors each round so comparisons stay paired).
3. Keep the top fraction (e.g. top 1/2 or 1/4) by mean DPS; drop the rest. Increase `b` (e.g. double it) for the survivors.
4. Repeat until few enough combos remain (e.g. ≤ top-N × small factor), then sim those to full precision.
5. **Final top-N re-sim:** re-sim the finalists with the *production* RNG path (not labeled rands) at full iterations so the **displayed absolute DPS is honest**, and so the final ordering doesn't depend on CRN-correlated noise.

Refinement (optional, later): replace fixed-fraction halving with **confidence-based racing** — drop a combo only when its CRN-based confidence interval is entirely below the current leader's. More iteration-efficient, more knobs/risk. Do this only if successive halving's accuracy/cost isn't good enough.

Implementation notes:
- The runner currently uses `dpsOnly` and trims per-iteration distributions. Adaptive needs per-combo **running mean + variance** retained across rounds — adjust what's collected.
- Re-gemming (the LP in `gem_lp.go`) is per-combo fixed overhead, independent of iterations. In a multi-round design, **gem each combo once and cache its gemmed equipment**, reusing it across rounds — don't re-solve the LP every round.
- Worker pool: each round sims a shrinking set; keep all cores busy (parallelize over the surviving combos × the round's iteration chunk).

**Gate:** on the Phase 0 fixed batch, reproduce the oracle's #1 and top-5 within the agreed confidence using a large reduction in total iterations. Report the iteration-cost ratio vs naive. If it ever picks a different #1 than the oracle, the elimination is too aggressive — back off the schedule.

---

## 5. Invariants & risks (do not violate)

- **Correctness beats speed.** A wrong "best gear" is a silent, serious failure. Every phase is graded against the Phase 0 oracle; never ship a schedule that flips the #1.
- **Never hard-cut at tiny iteration counts.** A good combo can look bad on an unlucky small sample. Eliminate by confidence / successive-halving, not a fixed early threshold.
- **Final displayed DPS must be honest** — produce it via a full-precision, production-RNG re-sim of the finalists, not CRN/labeled-rand estimates.
- **Don't regress the golden tests.** This work is in the batch runner; it must not change single-sim output. Run `GOTOOLCHAIN=go1.25.4 GOARCH=amd64 go test --tags=with_db ./sim/...` and confirm no golden `.results` change. If labeled-rands are used, keep them confined to the batch ranking path.
- **Concurrency gotcha (already bitten once):** combo `ItemSpec`s are shared across workers; the gem optimizer mutates `ItemSpec.Gems` in place. `applyComboToEquipment` clones per slot to avoid a data race that blanks sockets. If you cache gemmed equipment across rounds (Phase 3), keep each combo's equipment **worker-private / immutable once gemmed** — don't reintroduce shared mutable state.
- **Keep every phase toggleable** (env var or request flag) so you can A/B and roll back instantly.

---

## 6. Open questions (resolve empirically, don't assume)

- **Magnitude of CRN's win:** depends on how correlated combos actually are. Measure the variance-of-difference reduction (Phase 1/2). No number should be promised before measuring.
- **Labeled-rand cost:** the per-iteration slowdown vs the variance benefit is a genuine trade-off (Phase 2 gate).
- **Distribution of the combo field:** adaptive's win depends on how many combos are clearly-bad vs a tight top cluster. Measure on the user's real batches, not a synthetic one.
- **Interaction with the gem LP:** confirm gemming-once-and-caching is valid across rounds (the optimal gems depend on the combo's items + caps, not on iteration count, so it should be — verify).

---

## 7. Definition of done

On a representative real batch: reproduce the full-iteration oracle's top-5 (and specifically the #1) within the agreed confidence, at a large, measured reduction in total iterations; golden tests unchanged; final displayed DPS produced by a clean full-precision re-sim; all new behavior behind toggles; results validated by the user against a known-good manual ranking before it becomes the default.
