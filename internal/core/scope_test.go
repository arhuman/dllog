package core

import (
	"reflect"
	"testing"
)

func appendAll(t *testing.T, s *Scope[int], entries ...int) []Action {
	t.Helper()
	actions := make([]Action, 0, len(entries))
	for _, e := range entries {
		actions = append(actions, s.Append(e))
	}
	return actions
}

func TestNewScopeNormalizesCapacity(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		want     int
	}{
		{"negative", -7, DefaultCapacity},
		{"zero", 0, DefaultCapacity},
		{"one", 1, 1},
		{"explicit", 32, 32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScope[int](tt.capacity, 0)
			if got := s.Capacity(); got != tt.want {
				t.Fatalf("Capacity() = %d, want %d", got, tt.want)
			}
			// A normalized ring must actually buffer.
			if got := s.Append(1); got != ActionBuffered {
				t.Fatalf("Append() = %v, want ActionBuffered", got)
			}
		})
	}
}

func TestNewScopeNormalizesPostTripLimit(t *testing.T) {
	s := NewScope[int](4, -3)
	if _, _, ok := s.Trip(); !ok {
		t.Fatal("Trip() did not trip")
	}
	// A negative limit must behave as unlimited, not as "suppress everything".
	for i := range 5 {
		if got := s.Append(i); got != ActionPassThrough {
			t.Fatalf("Append(%d) = %v, want ActionPassThrough", i, got)
		}
	}
}

func TestRingUnderCapacityPreservesOrder(t *testing.T) {
	s := NewScope[int](8, 0)
	actions := appendAll(t, s, 1, 2, 3)
	for i, a := range actions {
		if a != ActionBuffered {
			t.Fatalf("Append #%d = %v, want ActionBuffered", i, a)
		}
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	entries, dropped, tripped := s.Trip()
	if !tripped {
		t.Fatal("Trip() tripped = false, want true")
	}
	if dropped != 0 {
		t.Fatalf("Trip() dropped = %d, want 0", dropped)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("Trip() entries = %v, want %v", entries, want)
	}
}

func TestRingEvictsOldest(t *testing.T) {
	tests := []struct {
		name        string
		capacity    int
		appended    int
		wantEntries []int
		wantDropped int
	}{
		{"exactly full", 3, 3, []int{0, 1, 2}, 0},
		{"one over", 3, 4, []int{1, 2, 3}, 1},
		{"wraps twice", 3, 8, []int{5, 6, 7}, 5},
		{"capacity one", 1, 4, []int{3}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScope[int](tt.capacity, 0)
			for i := range tt.appended {
				if got := s.Append(i); got != ActionBuffered {
					t.Fatalf("Append(%d) = %v, want ActionBuffered", i, got)
				}
			}
			if got := s.Len(); got != len(tt.wantEntries) {
				t.Fatalf("Len() = %d, want %d", got, len(tt.wantEntries))
			}
			if got := s.Dropped(); got != tt.wantDropped {
				t.Fatalf("Dropped() = %d, want %d", got, tt.wantDropped)
			}

			entries, dropped, tripped := s.Trip()
			if !tripped {
				t.Fatal("Trip() tripped = false, want true")
			}
			if !reflect.DeepEqual(entries, tt.wantEntries) {
				t.Fatalf("Trip() entries = %v, want %v", entries, tt.wantEntries)
			}
			if dropped != tt.wantDropped {
				t.Fatalf("Trip() dropped = %d, want %d", dropped, tt.wantDropped)
			}
		})
	}
}

func TestRingNeverGrows(t *testing.T) {
	s := NewScope[int](4, 0)
	for i := range 1000 {
		s.Append(i)
	}
	if got := s.Capacity(); got != 4 {
		t.Fatalf("Capacity() = %d, want 4 (ring must not grow)", got)
	}
	if got := s.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
	if got := s.Dropped(); got != 996 {
		t.Fatalf("Dropped() = %d, want 996", got)
	}
}

func TestTripIsIdempotent(t *testing.T) {
	s := NewScope[int](8, 0)
	appendAll(t, s, 1, 2)

	if s.Tripped() {
		t.Fatal("Tripped() = true before Trip()")
	}
	entries, dropped, tripped := s.Trip()
	if !tripped || len(entries) != 2 || dropped != 0 {
		t.Fatalf("first Trip() = (%v, %d, %v), want ([1 2], 0, true)", entries, dropped, tripped)
	}
	if !s.Tripped() {
		t.Fatal("Tripped() = false after Trip()")
	}

	entries, dropped, tripped = s.Trip()
	if tripped {
		t.Fatal("second Trip() tripped = true, want false")
	}
	if entries != nil {
		t.Fatalf("second Trip() entries = %v, want nil", entries)
	}
	if dropped != 0 {
		t.Fatalf("second Trip() dropped = %d, want 0", dropped)
	}
}

func TestTripDrainsBuffer(t *testing.T) {
	s := NewScope[int](8, 0)
	appendAll(t, s, 1, 2, 3)
	if _, _, ok := s.Trip(); !ok {
		t.Fatal("Trip() did not trip")
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("Len() after Trip() = %d, want 0", got)
	}
}

func TestAppendAfterTripPassesThrough(t *testing.T) {
	s := NewScope[int](8, 0)
	s.Append(1)
	if _, _, ok := s.Trip(); !ok {
		t.Fatal("Trip() did not trip")
	}

	if got := s.Append(2); got != ActionPassThrough {
		t.Fatalf("Append() after trip = %v, want ActionPassThrough", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0 (post-trip entries must not be buffered)", got)
	}
}

func TestPostTripLimit(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		appends int
		want    []Action
	}{
		{
			name: "unlimited", limit: 0, appends: 4,
			want: []Action{ActionPassThrough, ActionPassThrough, ActionPassThrough, ActionPassThrough},
		},
		{
			name: "budget of two", limit: 2, appends: 4,
			want: []Action{ActionPassThrough, ActionPassThrough, ActionSuppressed, ActionSuppressed},
		},
		{
			name: "budget of one", limit: 1, appends: 3,
			want: []Action{ActionPassThrough, ActionSuppressed, ActionSuppressed},
		},
		{
			name: "budget larger than traffic", limit: 10, appends: 2,
			want: []Action{ActionPassThrough, ActionPassThrough},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScope[int](8, tt.limit)
			// Pre-trip entries must not consume the post-trip budget.
			appendAll(t, s, 100, 101)
			if _, _, ok := s.Trip(); !ok {
				t.Fatal("Trip() did not trip")
			}

			got := make([]Action, 0, tt.appends)
			for i := range tt.appends {
				got = append(got, s.Append(i))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("post-trip actions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCloseMakesAppendPassThrough(t *testing.T) {
	s := NewScope[int](8, 0)
	s.Append(1)
	s.Close()

	if got := s.Append(2); got != ActionPassThrough {
		t.Fatalf("Append() after Close() = %v, want ActionPassThrough", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("Len() after Close() = %d, want 0", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := NewScope[int](8, 0)
	s.Append(1)
	s.Close()
	s.Close()
	s.Close()

	if got := s.Append(2); got != ActionPassThrough {
		t.Fatalf("Append() = %v, want ActionPassThrough", got)
	}
}

func TestCloseIgnoresPostTripBudget(t *testing.T) {
	s := NewScope[int](8, 1)
	if _, _, ok := s.Trip(); !ok {
		t.Fatal("Trip() did not trip")
	}
	s.Append(1) // spends the budget
	if got := s.Append(2); got != ActionSuppressed {
		t.Fatalf("Append() = %v, want ActionSuppressed", got)
	}
	s.Close()
	// After Close the scope behaves as if it never existed: no suppression.
	if got := s.Append(3); got != ActionPassThrough {
		t.Fatalf("Append() after Close() = %v, want ActionPassThrough", got)
	}
}

func TestTripAfterCloseDoesNotTrip(t *testing.T) {
	s := NewScope[int](8, 0)
	appendAll(t, s, 1, 2)
	s.Close()

	entries, dropped, tripped := s.Trip()
	if tripped {
		t.Fatal("Trip() after Close() tripped = true, want false")
	}
	if entries != nil || dropped != 0 {
		t.Fatalf("Trip() after Close() = (%v, %d), want (nil, 0)", entries, dropped)
	}
	if s.Tripped() {
		t.Fatal("Tripped() = true after a Trip() that did not trip")
	}
}

func TestCloseAfterTripIsSafe(t *testing.T) {
	s := NewScope[int](8, 0)
	appendAll(t, s, 1, 2)
	entries, _, tripped := s.Trip()
	if !tripped {
		t.Fatal("Trip() did not trip")
	}
	s.Close()

	// The flushed batch belongs to the caller; Close must not clobber it.
	if want := []int{1, 2}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries after Close() = %v, want %v", entries, want)
	}
	if got := s.Append(3); got != ActionPassThrough {
		t.Fatalf("Append() = %v, want ActionPassThrough", got)
	}
}

func TestTripEntriesAreNotAliasedByLaterAppends(t *testing.T) {
	s := NewScope[int](2, 0)
	appendAll(t, s, 1, 2)
	entries, _, _ := s.Trip()

	// Fresh scope reuse pattern: nothing the scope does afterwards may mutate
	// the batch handed to the caller.
	s2 := NewScope[int](2, 0)
	appendAll(t, s2, 9, 9)
	s.Append(7)

	if want := []int{1, 2}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
}

func TestGenericOverStructEntries(t *testing.T) {
	type entry struct {
		msg string
		n   int
	}
	s := NewScope[entry](2, 0)
	s.Append(entry{"a", 1})
	s.Append(entry{"b", 2})
	s.Append(entry{"c", 3})

	entries, dropped, tripped := s.Trip()
	if !tripped {
		t.Fatal("Trip() did not trip")
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	want := []entry{{"b", 2}, {"c", 3}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
}

func TestActionString(t *testing.T) {
	tests := []struct {
		action Action
		want   string
	}{
		{ActionBuffered, "ActionBuffered"},
		{ActionPassThrough, "ActionPassThrough"},
		{ActionSuppressed, "ActionSuppressed"},
		{Action(99), "Action(unknown)"},
	}
	for _, tt := range tests {
		if got := tt.action.String(); got != tt.want {
			t.Fatalf("Action(%d).String() = %q, want %q", tt.action, got, tt.want)
		}
	}
}
