package core

import "testing"

const benchAppends = 16

// BenchmarkScopeLifecycle measures the steady-state cost of one operation:
// create a scope, append to it, close it. The unpooled and pooled variants are
// the before/after for the pooling claim; the difference is the ring array.
func BenchmarkScopeLifecycle(b *testing.B) {
	b.Run("unpooled", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			s := NewScope[int](DefaultCapacity, 0)
			for i := range benchAppends {
				s.Append(i)
			}
			s.Close()
		}
	})

	b.Run("pooled", func(b *testing.B) {
		p := NewScopePool[int](DefaultCapacity, 0)
		b.ReportAllocs()
		for b.Loop() {
			s := p.Get()
			for i := range benchAppends {
				s.Append(i)
			}
			s.Close()
		}
	})

	// A tripped scope still returns its ring on Close, so the tripping path
	// must pool as well as the discard path.
	b.Run("pooled-tripped", func(b *testing.B) {
		p := NewScopePool[int](DefaultCapacity, 0)
		b.ReportAllocs()
		for b.Loop() {
			s := p.Get()
			for i := range benchAppends {
				s.Append(i)
			}
			s.Trip()
			s.Close()
		}
	})
}

// BenchmarkScopeLifecycleParallel checks that pooling still holds up when
// scopes are opened concurrently, which is the real HTTP-server shape.
func BenchmarkScopeLifecycleParallel(b *testing.B) {
	p := NewScopePool[int](DefaultCapacity, 0)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := p.Get()
			for i := range benchAppends {
				s.Append(i)
			}
			s.Close()
		}
	})
}
