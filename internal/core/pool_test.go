package core

import "testing"

func TestNewScopePoolNormalizesArguments(t *testing.T) {
	tests := []struct {
		name         string
		capacity     int
		wantCapacity int
	}{
		{"negative", -7, DefaultCapacity},
		{"zero", 0, DefaultCapacity},
		{"one", 1, 1},
		{"explicit", 32, 32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewScopePool[int](tt.capacity, 0)
			if got := p.Capacity(); got != tt.wantCapacity {
				t.Fatalf("pool Capacity() = %d, want %d", got, tt.wantCapacity)
			}
			s := p.Get()
			defer s.Close()
			if got := s.Capacity(); got != tt.wantCapacity {
				t.Fatalf("scope Capacity() = %d, want %d", got, tt.wantCapacity)
			}
			if got := s.Append(1); got != ActionBuffered {
				t.Fatalf("Append() = %v, want ActionBuffered", got)
			}
		})
	}
}

// A negative post-trip limit must mean unlimited, exactly as NewScope treats it.
func TestNewScopePoolNormalizesPostTripLimit(t *testing.T) {
	p := NewScopePool[int](4, -3)
	s := p.Get()
	defer s.Close()
	if _, _, ok := s.Trip(); !ok {
		t.Fatal("Trip() did not trip")
	}
	for i := range 5 {
		if got := s.Append(i); got != ActionPassThrough {
			t.Fatalf("Append(%d) = %v, want ActionPassThrough", i, got)
		}
	}
}

// put refuses a ring whose length does not match the pool, so a mismatched
// array can never be reissued as if it were the right size.
func TestPoolRejectsForeignRing(t *testing.T) {
	p := NewScopePool[int](8, 0)

	foreign := make([]int, 4)
	p.put(&foreign)
	p.put(nil)

	s := p.Get()
	defer s.Close()
	if got := s.Capacity(); got != 8 {
		t.Fatalf("Capacity() = %d, want 8", got)
	}
	// The scope must really have 8 usable slots, not the 4 of the foreign ring.
	for i := range 8 {
		if got := s.Append(i); got != ActionBuffered {
			t.Fatalf("Append(%d) = %v, want ActionBuffered", i, got)
		}
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0: the pool issued an undersized ring", got)
	}
	entries, _, tripped := s.Trip()
	if !tripped || len(entries) != 8 {
		t.Fatalf("Trip() returned %d entries, want 8", len(entries))
	}
}

// A scope from a pool must be reusable through a full lifecycle many times over,
// and Close on an already-closed pooled scope must not return its ring twice.
func TestPooledScopeCloseIsIdempotent(t *testing.T) {
	p := NewScopePool[int](8, 0)

	s := p.Get()
	s.Append(1)
	s.Close()
	s.Close()
	s.Close()

	// Two live scopes must own distinct rings; a double-returned ring would
	// hand the same array to both.
	a, b := p.Get(), p.Get()
	defer a.Close()
	defer b.Close()

	for i := range 8 {
		a.Append(i)
	}
	if got := b.Len(); got != 0 {
		t.Fatalf("second scope Len() = %d, want 0: two live scopes share one ring", got)
	}
}
