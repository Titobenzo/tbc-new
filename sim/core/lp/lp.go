// Package lp is a Go port of the YALPS mixed-integer linear-program solver
// (https://github.com/ Added-by-port, MIT). It implements a two-phase simplex with a
// branch-and-cut wrapper for integer/binary variables. The wowsims "Suggest Gems"/reforge
// optimizer in the TS UI uses YALPS; this port lets the native batch runner solve the exact
// same model so its gem choices match the browser's.
//
// Only the subset of YALPS the optimizer needs is ported (maximize/minimize, min/max/equal
// constraints, integer + binary variables). The numerics mirror YALPS exactly so results match.
package lp

import (
	"container/heap"
	"math"
	"time"
)

// epsilon mirrors JS Number.EPSILON, used by roundToPrecision.
const epsilon = 2.220446049250313e-16

// Constraint bounds a named row. Use the helpers below; Min == -Inf or Max == +Inf means unbounded
// on that side, Min == Max means an equality.
type Constraint struct {
	Min float64
	Max float64
}

func LessEq(v float64) Constraint    { return Constraint{Min: math.Inf(-1), Max: v} }
func GreaterEq(v float64) Constraint { return Constraint{Min: v, Max: math.Inf(1)} }
func EqualTo(v float64) Constraint   { return Constraint{Min: v, Max: v} }
func InRange(lo, hi float64) Constraint {
	return Constraint{Min: lo, Max: hi}
}

// ConstraintEntry keeps constraints ordered (Go maps are unordered; the tableau row layout must be
// deterministic, so we carry an ordered slice).
type ConstraintEntry struct {
	Key        string
	Constraint Constraint
}

// Variable is one column: its coefficient in the objective (under Model.Objective key) and in each
// constraint row it participates in.
type Variable struct {
	Key    string
	Coeffs map[string]float64
}

// Model is a linear program. Variables and Constraints are ordered for deterministic results.
type Model struct {
	Maximize    bool
	Objective   string
	Constraints []ConstraintEntry
	Variables   []Variable
	Binary      map[string]bool // variables constrained to {0,1}
	Integer     map[string]bool // variables constrained to integers
}

// Options tune the solver. Zero values fall back to YALPS defaults via withDefaults.
type Options struct {
	Precision     float64
	MaxPivots     int
	MaxIterations int
	Tolerance     float64
	Timeout       time.Duration
}

func (o Options) withDefaults() Options {
	if o.Precision == 0 {
		o.Precision = 1e-8
	}
	if o.MaxPivots == 0 {
		o.MaxPivots = 8192
	}
	if o.MaxIterations == 0 {
		o.MaxIterations = 32768
	}
	if o.Timeout == 0 {
		o.Timeout = time.Hour // YALPS default is Infinity; an hour is effectively unbounded here.
	}
	return o
}

// Solution is the result of Solve. Status is one of "optimal", "infeasible", "unbounded",
// "timedout", "cycled". Values holds the value of each variable with a value above precision.
type Solution struct {
	Status string
	Result float64
	Values map[string]float64
}

func isFinite(x float64) bool { return !math.IsInf(x, 0) && !math.IsNaN(x) }

func roundToPrecision(num, precision float64) float64 {
	rounding := math.Round(1.0 / precision)
	return math.Round((num+epsilon)*rounding) / rounding
}

// ---- tableau ----

type tableau struct {
	matrix             []float64
	width              int
	height             int
	positionOfVariable []int32
	variableAtPosition []int32
}

func (t *tableau) at(row, col int) float64    { return t.matrix[row*t.width+col] }
func (t *tableau) set(row, col int, v float64) { t.matrix[row*t.width+col] = v }

type tabmod struct {
	tab       *tableau
	sign      float64
	variables []Variable
	integers  []int // 1-indexed columns that must be integer
}

type bounds struct {
	row   int
	lower float64
	upper float64
}

func tableauModel(m *Model) *tabmod {
	sign := 1.0
	if !m.Maximize {
		sign = -1.0
	}
	vars := m.Variables

	var binaryCols []int
	var ints []int
	for i := 1; i <= len(vars); i++ {
		key := vars[i-1].Key
		if m.Binary[key] {
			binaryCols = append(binaryCols, i)
			ints = append(ints, i)
		} else if m.Integer[key] {
			ints = append(ints, i)
		}
	}

	order := make([]string, 0, len(m.Constraints))
	cmap := make(map[string]*bounds, len(m.Constraints))
	for _, ce := range m.Constraints {
		b, ok := cmap[ce.Key]
		if !ok {
			b = &bounds{row: -1, lower: math.Inf(-1), upper: math.Inf(1)}
			cmap[ce.Key] = b
			order = append(order, ce.Key)
		}
		if ce.Constraint.Min > b.lower {
			b.lower = ce.Constraint.Min
		}
		if ce.Constraint.Max < b.upper {
			b.upper = ce.Constraint.Max
		}
	}

	numConstraints := 1
	for _, key := range order {
		b := cmap[key]
		b.row = numConstraints
		if isFinite(b.upper) {
			numConstraints++
		}
		if isFinite(b.lower) {
			numConstraints++
		}
	}

	width := len(vars) + 1
	height := numConstraints + len(binaryCols)
	numVars := width + height
	t := &tableau{
		matrix:             make([]float64, width*height),
		width:              width,
		height:             height,
		positionOfVariable: make([]int32, numVars),
		variableAtPosition: make([]int32, numVars),
	}
	for i := 0; i < numVars; i++ {
		t.positionOfVariable[i] = int32(i)
		t.variableAtPosition[i] = int32(i)
	}

	for c := 1; c < width; c++ {
		for ck, coef := range vars[c-1].Coeffs {
			if ck == m.Objective {
				t.set(0, c, sign*coef)
			}
			if b, ok := cmap[ck]; ok {
				if isFinite(b.upper) {
					t.set(b.row, c, coef)
					if isFinite(b.lower) {
						t.set(b.row+1, c, -coef)
					}
				} else if isFinite(b.lower) {
					t.set(b.row, c, -coef)
				}
			}
		}
	}

	for _, key := range order {
		b := cmap[key]
		if isFinite(b.upper) {
			t.set(b.row, 0, b.upper)
			if isFinite(b.lower) {
				t.set(b.row+1, 0, -b.lower)
			}
		} else if isFinite(b.lower) {
			t.set(b.row, 0, -b.lower)
		}
	}

	for bi, col := range binaryCols {
		row := numConstraints + bi
		t.set(row, 0, 1.0)
		t.set(row, col, 1.0)
	}

	return &tabmod{tab: t, sign: sign, variables: vars, integers: ints}
}

// ---- simplex ----

func pivot(t *tableau, row, col int) {
	quotient := t.at(row, col)
	leaving := t.variableAtPosition[t.width+row]
	entering := t.variableAtPosition[col]
	t.variableAtPosition[t.width+row] = entering
	t.variableAtPosition[col] = leaving
	t.positionOfVariable[leaving] = int32(col)
	t.positionOfVariable[entering] = int32(t.width + row)

	nonZero := make([]int, 0, t.width)
	for c := 0; c < t.width; c++ {
		v := t.at(row, c)
		if math.Abs(v) > 1e-16 {
			t.set(row, c, v/quotient)
			nonZero = append(nonZero, c)
		} else {
			t.set(row, c, 0.0)
		}
	}
	t.set(row, col, 1.0/quotient)

	for r := 0; r < t.height; r++ {
		if r == row {
			continue
		}
		coef := t.at(r, col)
		if math.Abs(coef) > 1e-16 {
			for _, c := range nonZero {
				t.set(r, c, t.at(r, c)-coef*t.at(row, c))
			}
			t.set(r, col, -coef/quotient)
		}
	}
}

func phase2(t *tableau, prec float64, maxPivots int) (string, float64) {
	for iter := 0; iter < maxPivots; iter++ {
		col := 0
		value := prec
		for c := 1; c < t.width; c++ {
			rc := t.at(0, c)
			if rc > value {
				value = rc
				col = c
			}
		}
		if col == 0 {
			return "optimal", roundToPrecision(t.at(0, 0), prec)
		}
		row := 0
		minRatio := math.Inf(1)
		for r := 1; r < t.height; r++ {
			v := t.at(r, col)
			if v <= prec {
				continue
			}
			ratio := t.at(r, 0) / v
			if ratio < minRatio {
				row = r
				minRatio = ratio
				if ratio <= prec {
					break
				}
			}
		}
		if row == 0 {
			return "unbounded", float64(col)
		}
		pivot(t, row, col)
	}
	return "cycled", math.NaN()
}

func phase1(t *tableau, prec float64, maxPivots int) (string, float64) {
	for iter := 0; iter < maxPivots; iter++ {
		row := 0
		rhs := -prec
		for r := 1; r < t.height; r++ {
			v := t.at(r, 0)
			if v < rhs {
				rhs = v
				row = r
			}
		}
		if row == 0 {
			return phase2(t, prec, maxPivots)
		}
		col := 0
		maxRatio := math.Inf(-1)
		for c := 1; c < t.width; c++ {
			coef := t.at(row, c)
			if coef < -prec {
				ratio := -t.at(0, c) / coef
				if ratio > maxRatio {
					maxRatio = ratio
					col = c
				}
			}
		}
		if col == 0 {
			return "infeasible", math.NaN()
		}
		pivot(t, row, col)
	}
	return "cycled", math.NaN()
}

// ---- branch and cut ----

type cut struct {
	sign     int // +1 (floor / <=) or -1 (ceil / >=)
	variable int
	value    float64
}

// applyCuts builds a fresh tableau from the root tableau plus the given cut rows, expressing each cut
// in terms of the root's current basis (mirrors YALPS applyCuts but allocates rather than reusing a
// shared buffer, which keeps the best-found tableau valid without YALPS's buffer-swap bookkeeping).
func applyCuts(root *tableau, cuts []cut) *tableau {
	width := root.width
	height := root.height
	newHeight := height + len(cuts)
	nt := &tableau{
		matrix:             make([]float64, width*newHeight),
		width:              width,
		height:             newHeight,
		positionOfVariable: make([]int32, width+newHeight),
		variableAtPosition: make([]int32, width+newHeight),
	}
	copy(nt.matrix, root.matrix)
	for i, c := range cuts {
		r := (height + i) * width
		pos := int(root.positionOfVariable[c.variable])
		if pos < width {
			nt.matrix[r] = float64(c.sign) * c.value
			for k := r + 1; k < r+width; k++ {
				nt.matrix[k] = 0.0
			}
			nt.matrix[r+pos] = float64(c.sign)
		} else {
			row := (pos - width) * width
			nt.matrix[r] = float64(c.sign) * (c.value - root.matrix[row])
			for cc := 1; cc < width; cc++ {
				nt.matrix[r+cc] = -float64(c.sign) * root.matrix[row+cc]
			}
		}
	}
	copy(nt.positionOfVariable, root.positionOfVariable)
	copy(nt.variableAtPosition, root.variableAtPosition)
	for i := width + height; i < width+newHeight; i++ {
		nt.positionOfVariable[i] = int32(i)
		nt.variableAtPosition[i] = int32(i)
	}
	return nt
}

func mostFractionalVar(t *tableau, intVars []int) (int, float64, float64) {
	highestFrac := 0.0
	variable := 0
	value := 0.0
	for _, iv := range intVars {
		row := int(t.positionOfVariable[iv]) - t.width
		if row < 0 {
			continue
		}
		val := t.at(row, 0)
		frac := math.Abs(val - math.Round(val))
		if frac > highestFrac {
			highestFrac = frac
			variable = iv
			value = val
		}
	}
	return variable, value, highestFrac
}

type branch struct {
	eval float64
	cuts []cut
}

type branchHeap []branch

func (h branchHeap) Len() int            { return len(h) }
func (h branchHeap) Less(i, j int) bool  { return h[i].eval < h[j].eval }
func (h branchHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *branchHeap) Push(x interface{}) { *h = append(*h, x.(branch)) }
func (h *branchHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

func branchAndCut(tm *tabmod, initResult float64, opt Options) (*tableau, string, float64) {
	root := tm.tab
	prec := opt.Precision

	initVariable, initValue, initFrac := mostFractionalVar(root, tm.integers)
	if initFrac <= prec {
		return root, "optimal", initResult
	}

	branches := &branchHeap{}
	heap.Init(branches)
	heap.Push(branches, branch{eval: initResult, cuts: []cut{{sign: -1, variable: initVariable, value: math.Ceil(initValue)}}})
	heap.Push(branches, branch{eval: initResult, cuts: []cut{{sign: 1, variable: initVariable, value: math.Floor(initValue)}}})

	optimalThreshold := initResult * (1.0 - tm.sign*opt.Tolerance)
	stopTime := time.Now().Add(opt.Timeout)
	timedout := !time.Now().Before(stopTime)
	solutionFound := false
	bestEval := math.Inf(1)
	bestTableau := root
	iter := 0

	for iter < opt.MaxIterations && branches.Len() > 0 && bestEval >= optimalThreshold && !timedout {
		b := heap.Pop(branches).(branch)
		if b.eval > bestEval {
			break
		}
		currentTableau := applyCuts(root, b.cuts)
		status, result := phase1(currentTableau, prec, opt.MaxPivots)
		if status == "optimal" && result < bestEval {
			variable, value, frac := mostFractionalVar(currentTableau, tm.integers)
			if frac <= prec {
				solutionFound = true
				bestEval = result
				bestTableau = currentTableau
			} else {
				cutsUpper := make([]cut, 0, len(b.cuts)+1)
				cutsLower := make([]cut, 0, len(b.cuts)+1)
				for _, c := range b.cuts {
					if c.variable == variable {
						if c.sign < 0 {
							cutsLower = append(cutsLower, c)
						} else {
							cutsUpper = append(cutsUpper, c)
						}
					} else {
						cutsUpper = append(cutsUpper, c)
						cutsLower = append(cutsLower, c)
					}
				}
				cutsLower = append(cutsLower, cut{sign: 1, variable: variable, value: math.Floor(value)})
				cutsUpper = append(cutsUpper, cut{sign: -1, variable: variable, value: math.Ceil(value)})
				heap.Push(branches, branch{eval: result, cuts: cutsUpper})
				heap.Push(branches, branch{eval: result, cuts: cutsLower})
			}
		}
		timedout = !time.Now().Before(stopTime)
		iter++
	}

	unfinished := (timedout || iter >= opt.MaxIterations) && branches.Len() > 0 && bestEval >= optimalThreshold
	status := "optimal"
	if unfinished {
		status = "timedout"
	} else if !solutionFound {
		status = "infeasible"
	}
	if solutionFound {
		return bestTableau, status, bestEval
	}
	return bestTableau, status, math.NaN()
}

// ---- solve ----

func makeSolution(tm *tabmod, status string, result float64, prec float64) Solution {
	t := tm.tab
	if status == "optimal" || (status == "timedout" && !math.IsNaN(result)) {
		values := make(map[string]float64)
		for i := 0; i < len(tm.variables); i++ {
			row := int(t.positionOfVariable[i+1]) - t.width
			value := 0.0
			if row >= 0 {
				value = t.at(row, 0)
			}
			if value > prec {
				values[tm.variables[i].Key] = roundToPrecision(value, prec)
			}
		}
		return Solution{Status: status, Result: -tm.sign * result, Values: values}
	}
	return Solution{Status: status, Result: math.NaN(), Values: map[string]float64{}}
}

// Solve runs the LP/MILP solver on the model and returns the optimal variable assignment.
func Solve(m *Model, opts ...Options) Solution {
	opt := Options{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt = opt.withDefaults()

	tm := tableauModel(m)
	status, result := phase1(tm.tab, opt.Precision, opt.MaxPivots)
	if len(tm.integers) == 0 || status != "optimal" {
		return makeSolution(tm, status, result, opt.Precision)
	}
	bestTab, istatus, iresult := branchAndCut(tm, result, opt)
	return makeSolution(&tabmod{tab: bestTab, sign: tm.sign, variables: tm.variables, integers: tm.integers}, istatus, iresult, opt.Precision)
}
