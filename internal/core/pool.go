package core

import "sync"

// ScopePool recycles the ring arrays of the scopes it creates, so a program
// that opens and closes one scope per operation stops allocating a ring per
// operation once it reaches steady state.
//
// A pool is fixed to one entry type and one capacity. That is what makes
// recycling safe: a ring can only ever be handed back to the pool it came
// from, so a capacity-8 ring can never satisfy a request for 256, and a
// []string ring can never reach a Scope[int].
//
// The type exists because Go has no generic package-level variables: a
// sync.Pool holding []T cannot be a package-level var, and the alternatives
// (one untyped pool for every T, or a reflect-keyed registry) either thrash
// when two entry types interleave or pay a map lookup per scope. Hanging the
// pool off a value the caller already owns costs neither. Callers that create
// scopes from a long-lived owner, which is the intended use, get pooling for
// free; NewScope remains available and unpooled for one-off scopes.
//
// A ScopePool is safe for concurrent use. Its zero value is not usable; call
// NewScopePool.
type ScopePool[T any] struct {
	capacity      int
	postTripLimit int
	// pool holds *[]T rather than []T: a pointer fits in an interface word,
	// so Put does not allocate to box the slice header.
	pool sync.Pool
}

// NewScopePool returns a pool that builds scopes of the given capacity and
// post-trip budget. Both arguments are normalized exactly as NewScope
// normalizes them: capacity <= 0 becomes DefaultCapacity, a negative
// postTripLimit becomes 0.
func NewScopePool[T any](capacity, postTripLimit int) *ScopePool[T] {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if postTripLimit < 0 {
		postTripLimit = 0
	}
	return &ScopePool[T]{capacity: capacity, postTripLimit: postTripLimit}
}

// Capacity returns the ring size every scope from this pool is built with.
func (p *ScopePool[T]) Capacity() int { return p.capacity }

// Get returns a scope backed by a recycled ring when one is available and a
// fresh one otherwise. The scope returns its ring to the pool on Close; a
// scope that is never closed simply lets its ring be collected.
func (p *ScopePool[T]) Get() *Scope[T] {
	held := p.getRing()
	return &Scope[T]{
		capacity: p.capacity,
		ring:     *held,
		held:     held,
		limit:    p.postTripLimit,
		pool:     p,
	}
}

// getRing takes a ring box from the pool, or builds one. Rings are stored fully
// zeroed by put, so the caller receives a ring indistinguishable from make.
//
// The *[]T box is recycled along with the array it points at. Handing the same
// box back to put is what keeps Put from allocating: taking &ring on a fresh
// local would escape a new slice header to the heap on every Close.
func (p *ScopePool[T]) getRing() *[]T {
	if held, ok := p.pool.Get().(*[]T); ok {
		return held
	}
	ring := make([]T, p.capacity)
	return &ring
}

// put returns a ring box to the pool. The caller must already have cleared the
// array and dropped every other reference to it.
func (p *ScopePool[T]) put(held *[]T) {
	if held == nil || len(*held) != p.capacity {
		return
	}
	p.pool.Put(held)
}
