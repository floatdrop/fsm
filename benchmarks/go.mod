// A separate module so the root stays dependency-free: vendoring
// github.com/floatdrop/fsm pulls none of this in.
module github.com/floatdrop/fsm/benchmarks

go 1.27

require (
	github.com/floatdrop/fsm v0.5.1
	github.com/looplab/fsm v1.0.4
	github.com/qmuntal/stateless v1.8.0
)

replace github.com/floatdrop/fsm => ../
