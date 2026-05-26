package lp

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Continuous LP: maximize 3x + 2y s.t. x+y<=4, x+3y<=6 (x,y>=0). Optimum is x=4, y=0, obj=12.
func TestSolveContinuousLP(t *testing.T) {
	m := &Model{
		Maximize:  true,
		Objective: "obj",
		Constraints: []ConstraintEntry{
			{"c1", LessEq(4)},
			{"c2", LessEq(6)},
		},
		Variables: []Variable{
			{Key: "x", Coeffs: map[string]float64{"obj": 3, "c1": 1, "c2": 1}},
			{Key: "y", Coeffs: map[string]float64{"obj": 2, "c1": 1, "c2": 3}},
		},
	}
	sol := Solve(m)
	if sol.Status != "optimal" {
		t.Fatalf("status = %q, want optimal", sol.Status)
	}
	if !approx(sol.Result, 12) {
		t.Errorf("result = %v, want 12", sol.Result)
	}
	if !approx(sol.Values["x"], 4) {
		t.Errorf("x = %v, want 4", sol.Values["x"])
	}
	if v, ok := sol.Values["y"]; ok && !approx(v, 0) {
		t.Errorf("y = %v, want 0", v)
	}
}

// Binary MILP: maximize 5a+4b+3c s.t. 2a+3b+c<=5, 4a+b+2c<=11, 3a+4b+2c<=8, a,b,c in {0,1}.
// Optimum is a=1, b=1, c=0, obj=9 (the LP relaxation is fractional, so this exercises branch&cut).
func TestSolveBinaryMILP(t *testing.T) {
	m := &Model{
		Maximize:  true,
		Objective: "obj",
		Constraints: []ConstraintEntry{
			{"w1", LessEq(5)},
			{"w2", LessEq(11)},
			{"w3", LessEq(8)},
		},
		Variables: []Variable{
			{Key: "a", Coeffs: map[string]float64{"obj": 5, "w1": 2, "w2": 4, "w3": 3}},
			{Key: "b", Coeffs: map[string]float64{"obj": 4, "w1": 3, "w2": 1, "w3": 4}},
			{Key: "c", Coeffs: map[string]float64{"obj": 3, "w1": 1, "w2": 2, "w3": 2}},
		},
		Binary: map[string]bool{"a": true, "b": true, "c": true},
	}
	sol := Solve(m)
	if sol.Status != "optimal" {
		t.Fatalf("status = %q, want optimal", sol.Status)
	}
	if !approx(sol.Result, 9) {
		t.Errorf("result = %v, want 9", sol.Result)
	}
	if !approx(sol.Values["a"], 1) || !approx(sol.Values["b"], 1) {
		t.Errorf("a=%v b=%v, want a=1 b=1", sol.Values["a"], sol.Values["b"])
	}
	if v, ok := sol.Values["c"]; ok && !approx(v, 0) {
		t.Errorf("c = %v, want 0", v)
	}
}

// Equality "pick exactly one" constraint plus a >= constraint, like the gem model uses per socket.
// maximize 10p + 7q + 4r s.t. p+q+r == 1 (binary), and a "min red" style q+r >= 1. Best is q (7).
func TestSolvePickOneWithFloor(t *testing.T) {
	m := &Model{
		Maximize:  true,
		Objective: "obj",
		Constraints: []ConstraintEntry{
			{"socket", EqualTo(1)},
			{"floor", GreaterEq(1)},
		},
		Variables: []Variable{
			{Key: "p", Coeffs: map[string]float64{"obj": 10, "socket": 1}},
			{Key: "q", Coeffs: map[string]float64{"obj": 7, "socket": 1, "floor": 1}},
			{Key: "r", Coeffs: map[string]float64{"obj": 4, "socket": 1, "floor": 1}},
		},
		Binary: map[string]bool{"p": true, "q": true, "r": true},
	}
	sol := Solve(m)
	if sol.Status != "optimal" {
		t.Fatalf("status = %q, want optimal", sol.Status)
	}
	// p alone (10) violates the floor; the floor forces q or r, best is q => obj 7.
	if !approx(sol.Result, 7) {
		t.Errorf("result = %v, want 7", sol.Result)
	}
	if !approx(sol.Values["q"], 1) {
		t.Errorf("q = %v, want 1", sol.Values["q"])
	}
}

// Infeasible model: x <= 1 but x >= 3.
func TestSolveInfeasible(t *testing.T) {
	m := &Model{
		Maximize:  true,
		Objective: "obj",
		Constraints: []ConstraintEntry{
			{"hi", LessEq(1)},
			{"lo", GreaterEq(3)},
		},
		Variables: []Variable{
			{Key: "x", Coeffs: map[string]float64{"obj": 1, "hi": 1, "lo": 1}},
		},
	}
	sol := Solve(m)
	if sol.Status != "infeasible" {
		t.Errorf("status = %q, want infeasible", sol.Status)
	}
}
