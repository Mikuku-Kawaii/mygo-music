package diagnostics

import (
	"math"
	"runtime/metrics"
)

// Runtime is process-wide Go memory and scheduling evidence. SystemBytes
// counts Go's reserved address space, separate from resident memory or GPU use.
type Runtime struct {
	Goroutines                                   int
	HeapObjects, HeapBytes, HeapReservedBytes    uint64
	StackBytes, SystemBytes, TotalAllocatedBytes uint64
	GCCycles                                     uint64
	Scheduler, GCCyclePauses                     *HistogramSummary `json:",omitempty"`
}

type HistogramSummary struct {
	Count                      uint64
	P95UpperNS, HighestUpperNS float64 `json:",omitempty"`
}

func SampleRuntime() Runtime {
	// ReadMemStats pauses the world. Runtime metrics suffice for an export
	// taken during playback and do not request a synchronous memory census.
	samples := []metrics.Sample{
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/gc/heap/objects:objects"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/heap/stacks:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/sched/latencies:seconds"},
		{Name: "/sched/pauses/total/gc:seconds"},
	}
	metrics.Read(samples)
	value := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	return Runtime{
		Goroutines: int(value(0)), HeapObjects: value(1), HeapBytes: value(2),
		HeapReservedBytes: value(2) + value(3) + value(4) + value(5),
		StackBytes:        value(6), SystemBytes: value(7), TotalAllocatedBytes: value(8), GCCycles: value(9),
		Scheduler: histogram(samples[10].Value), GCCyclePauses: histogram(samples[11].Value),
	}
}

func histogram(v metrics.Value) *HistogramSummary {
	if v.Kind() != metrics.KindFloat64Histogram {
		return nil
	}
	h := v.Float64Histogram()
	var total uint64
	for _, n := range h.Counts {
		total += n
	}
	if total == 0 {
		return nil
	}
	r := &HistogramSummary{Count: total}
	target := total - total/20
	var observed uint64
	for i, n := range h.Counts {
		if n == 0 {
			continue
		}
		upper := h.Buckets[i+1] * 1e9
		if math.IsInf(upper, 0) {
			upper = 0 // an open-ended bucket has no numerical upper bound
		}
		if observed < target && observed+n >= target {
			r.P95UpperNS = upper
		}
		r.HighestUpperNS = upper
		observed += n
	}
	return r
}
