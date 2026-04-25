//go:build !race

package taskstore

// raceEnabled is false in non-race builds. The warm-List perf assertion
// uses this to pick a 1ms bound under normal builds and a 10ms bound
// under -race (allocator overhead is 5-10x with the race detector on).
const raceEnabled = false
