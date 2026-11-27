package application

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/internal/hash"
)

// filterTestService is the happy/failure fixture: three ordinary methods plus
// the optional lifecycle methods that must never reach the filter.
type filterTestService struct{}

func (s *filterTestService) AllowedAlpha() string { return "alpha" }
func (s *filterTestService) AllowedBeta() string  { return "beta" }
func (s *filterTestService) DeniedGamma() string  { return "gamma" }

func (s *filterTestService) ServiceStartup(context.Context, ServiceOptions) error { return nil }
func (s *filterTestService) ServiceShutdown() error                               { return nil }

// filterEmptyService has no exported methods.
type filterEmptyService struct{}

var (
	_ ServiceStartup  = (*filterTestService)(nil)
	_ ServiceShutdown = (*filterTestService)(nil)
)

// useFilterTestGlobalApplication installs a bare App so Bindings.Add's debug
// logging has a receiver. The package runs tests sequentially (no t.Parallel
// here), so restoring in cleanup is safe.
func useFilterTestGlobalApplication(t *testing.T) {
	t.Helper()
	prev := globalApplication
	globalApplication = &App{}
	t.Cleanup(func() { globalApplication = prev })
}

func filterTestFQN(methodName string) string {
	t := reflect.TypeOf(filterTestService{})
	return fmt.Sprintf("%s.%s.%s", t.PkgPath(), t.Name(), methodName)
}

func filterTestID(methodName string) uint32 {
	return hash.Fnv(filterTestFQN(methodName))
}

// assertBound checks that a method is registered under its expected FQN and
// computed ID, and that both lookup paths agree. It also proves the filter left
// unfiltered FQNs and IDs unchanged.
func assertBound(t *testing.T, b *Bindings, methodName string) *BoundMethod {
	t.Helper()

	fqn := filterTestFQN(methodName)
	wantID := filterTestID(methodName)

	bm := b.Get(&CallOptions{MethodName: fqn})
	if bm == nil {
		t.Fatalf("Get(%q) = nil, want bound method", fqn)
	}
	if bm.Name != methodName {
		t.Errorf("bound Name = %q, want %q", bm.Name, methodName)
	}
	if bm.FQN != fqn {
		t.Errorf("bound FQN = %q, want %q", bm.FQN, fqn)
	}
	if bm.ID != wantID {
		t.Errorf("bound ID = %d, want hash %d (FQN/ID must not change)", bm.ID, wantID)
	}

	stored, ok := b.boundMethods[fqn]
	if !ok || stored != bm {
		t.Errorf("boundMethods[%q] missing or mismatched", fqn)
	}

	byID := b.GetByID(bm.ID)
	if byID != bm {
		t.Errorf("GetByID(%d) = %v, want the same method as by-name lookup", bm.ID, byID)
	}
	if storedByID, ok := b.boundByID[bm.ID]; !ok || storedByID != bm {
		t.Errorf("boundByID[%d] missing or mismatched", bm.ID)
	}

	return bm
}

// assertFilteredOut checks that a denied method is absent from both lookup
// paths and from both backing maps, and that no lookup panics.
func assertFilteredOut(t *testing.T, b *Bindings, methodName string) {
	t.Helper()

	fqn := filterTestFQN(methodName)
	id := filterTestID(methodName)

	if bm := b.Get(&CallOptions{MethodName: fqn}); bm != nil {
		t.Errorf("Get(%q) = %v, want nil for filtered-out method", fqn, bm)
	}
	if _, ok := b.boundMethods[fqn]; ok {
		t.Errorf("boundMethods contains filtered-out FQN %q", fqn)
	}
	if bm := b.GetByID(id); bm != nil {
		t.Errorf("GetByID(%d) = %v, want nil for filtered-out method", id, bm)
	}
	if _, ok := b.boundByID[id]; ok {
		t.Errorf("boundByID contains filtered-out ID %d", id)
	}
}

func TestBindingsMethodFilterNilBindsAll(t *testing.T) {
	useFilterTestGlobalApplication(t)
	bindings := NewBindings(nil, nil)
	service := NewServiceWithOptions(&filterTestService{}, ServiceOptions{Name: "filter-test"})

	if err := bindings.Add(service); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, name := range []string{"AllowedAlpha", "AllowedBeta", "DeniedGamma"} {
		assertBound(t, bindings, name)
	}
	if got := len(bindings.boundMethods); got != 3 {
		t.Errorf("len(boundMethods) = %d, want 3", got)
	}
	if got := len(bindings.boundByID); got != 3 {
		t.Errorf("len(boundByID) = %d, want 3", got)
	}

	// Lifecycle methods are internal and never bound, filtered or not.
	for _, name := range []string{"ServiceStartup", "ServiceShutdown"} {
		if bm := bindings.Get(&CallOptions{MethodName: filterTestFQN(name)}); bm != nil {
			t.Errorf("Get(%q) = %v, want nil: lifecycle methods are not bound", name, bm)
		}
	}

	// An empty service still binds cleanly with a nil filter.
	empty := NewBindings(nil, nil)
	if err := empty.Add(NewService(&filterEmptyService{})); err != nil {
		t.Fatalf("Add(empty): %v", err)
	}
	if len(empty.boundMethods) != 0 || len(empty.boundByID) != 0 {
		t.Errorf("empty service bound methods: byName=%d byID=%d, want 0/0", len(empty.boundMethods), len(empty.boundByID))
	}
}

func TestBindingsMethodFilterBareNameContract(t *testing.T) {
	useFilterTestGlobalApplication(t)
	var seen []string
	keep := map[string]bool{"AllowedAlpha": true, "AllowedBeta": true}

	bindings := NewBindings(nil, nil)
	service := NewServiceWithOptions(&filterTestService{}, ServiceOptions{
		MethodFilter: func(methodName string) bool {
			seen = append(seen, methodName)
			return keep[methodName]
		},
	})

	if err := bindings.Add(service); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The filter must see the bare Go method names, never FQNs.
	sort.Strings(seen)
	want := []string{"AllowedAlpha", "AllowedBeta", "DeniedGamma"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("filter saw %v, want bare method names %v", seen, want)
	}
	for _, name := range seen {
		if name == "" {
			t.Error("filter received an empty method name")
		}
		if strings.Contains(name, ".") {
			t.Errorf("filter received %q, which looks like an FQN; want bare method name", name)
		}
	}

	// Allowed methods resolve by name and by ID and remain callable.
	for _, name := range []string{"AllowedAlpha", "AllowedBeta"} {
		bm := assertBound(t, bindings, name)
		result, err := bm.Call(context.Background(), nil)
		if err != nil {
			t.Fatalf("Call(%s): %v", name, err)
		}
		wantResult := strings.ToLower(strings.TrimPrefix(name, "Allowed"))
		if result != wantResult {
			t.Errorf("Call(%s) = %v, want %q", name, result, wantResult)
		}
	}

	// The denied method is gone from both paths and both maps, without panic.
	assertFilteredOut(t, bindings, "DeniedGamma")

	if got := len(bindings.boundMethods); got != 2 {
		t.Errorf("len(boundMethods) = %d, want 2", got)
	}
	if got := len(bindings.boundByID); got != 2 {
		t.Errorf("len(boundByID) = %d, want 2", got)
	}
}

// TestBindingsMethodFilterFQNKeyIgnored pins the contract: a filter keyed by
// the FQN-shaped name does not keep the method, because the filter never
// receives FQNs.
func TestBindingsMethodFilterFQNKeyIgnored(t *testing.T) {
	useFilterTestGlobalApplication(t)
	bindings := NewBindings(nil, nil)
	service := NewServiceWithOptions(&filterTestService{}, ServiceOptions{
		MethodFilter: func(methodName string) bool {
			return methodName == filterTestFQN("DeniedGamma")
		},
	})

	if err := bindings.Add(service); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, name := range []string{"AllowedAlpha", "AllowedBeta", "DeniedGamma"} {
		assertFilteredOut(t, bindings, name)
	}
	if len(bindings.boundMethods) != 0 || len(bindings.boundByID) != 0 {
		t.Errorf("FQN-keyed filter bound methods: byName=%d byID=%d, want 0/0", len(bindings.boundMethods), len(bindings.boundByID))
	}
}

func TestBindingsMethodFilterLifecycleUnaffected(t *testing.T) {
	useFilterTestGlobalApplication(t)
	var seen []string
	bindings := NewBindings(nil, nil)
	service := NewServiceWithOptions(&filterTestService{}, ServiceOptions{
		MethodFilter: func(methodName string) bool {
			seen = append(seen, methodName)
			return false
		},
	})

	if err := bindings.Add(service); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, name := range seen {
		if name == "ServiceStartup" || name == "ServiceShutdown" || name == "ServiceName" || name == "ServeHTTP" {
			t.Errorf("filter was consulted for internal service method %q", name)
		}
	}
	if len(bindings.boundMethods) != 0 || len(bindings.boundByID) != 0 {
		t.Errorf("deny-all filter bound methods: byName=%d byID=%d, want 0/0", len(bindings.boundMethods), len(bindings.boundByID))
	}

	// The lifecycle methods themselves still work.
	instance := &filterTestService{}
	if err := instance.ServiceStartup(context.Background(), DefaultServiceOptions); err != nil {
		t.Errorf("ServiceStartup: %v", err)
	}
	if err := instance.ServiceShutdown(); err != nil {
		t.Errorf("ServiceShutdown: %v", err)
	}
}

type filterDupServiceA struct{}

func (s *filterDupServiceA) CollideAlpha() string { return "a" }

type filterDupServiceB struct{}

func (s *filterDupServiceB) CollideBeta() string { return "b" }

// TestBindingsMethodFilterDuplicateIDs proves duplicate-ID validation runs
// after filtering: a filtered-out method with a colliding stable ID neither
// conflicts with nor appears in the registry.
func TestBindingsMethodFilterDuplicateIDs(t *testing.T) {
	useFilterTestGlobalApplication(t)
	const sharedID uint32 = 0x0BADF00D

	registerA := func() { RegisterBindingMethodID((*filterDupServiceA).CollideAlpha, sharedID) }
	unregisterA := func() { UnregisterBindingMethodID((*filterDupServiceA).CollideAlpha) }
	registerB := func() { RegisterBindingMethodID((*filterDupServiceB).CollideBeta, sharedID) }
	unregisterB := func() { UnregisterBindingMethodID((*filterDupServiceB).CollideBeta) }

	registerA()
	registerB()
	t.Cleanup(func() {
		unregisterA()
		unregisterB()
	})

	fqnA := reflect.TypeOf(filterDupServiceA{}).PkgPath() + ".filterDupServiceA.CollideAlpha"
	fqnB := reflect.TypeOf(filterDupServiceB{}).PkgPath() + ".filterDupServiceB.CollideBeta"

	// Sanity: with no filter, the shared ID is a collision.
	unfiltered := NewBindings(nil, nil)
	if err := unfiltered.Add(NewService(&filterDupServiceA{})); err != nil {
		t.Fatalf("Add(A, no filter): %v", err)
	}
	if err := unfiltered.Add(NewService(&filterDupServiceB{})); err == nil {
		t.Fatal("Add(B, no filter) = nil error, want duplicate-ID collision")
	}

	// Filtering out the colliding method prevents the collision entirely.
	bindings := NewBindings(nil, nil)
	if err := bindings.Add(NewServiceWithOptions(&filterDupServiceA{}, ServiceOptions{})); err != nil {
		t.Fatalf("Add(A): %v", err)
	}
	filteredB := NewServiceWithOptions(&filterDupServiceB{}, ServiceOptions{
		MethodFilter: func(string) bool { return false },
	})
	if err := bindings.Add(filteredB); err != nil {
		t.Fatalf("Add(filtered B) = %v, want nil (filtered method must not collide)", err)
	}

	bmA := bindings.Get(&CallOptions{MethodName: fqnA})
	if bmA == nil || bmA.ID != sharedID {
		t.Fatalf("Get(%q) = %v, want method with registered ID %d", fqnA, bmA, sharedID)
	}
	if bm := bindings.Get(&CallOptions{MethodName: fqnB}); bm != nil {
		t.Errorf("Get(%q) = %v, want nil", fqnB, bm)
	}
	if _, ok := bindings.boundMethods[fqnB]; ok {
		t.Errorf("boundMethods contains filtered-out %q", fqnB)
	}
	if bm := bindings.GetByID(sharedID); bm != bmA {
		t.Errorf("GetByID(%d) = %v, want the allowed method %v", sharedID, bm, bmA)
	}
	if len(bindings.boundMethods) != 1 || len(bindings.boundByID) != 1 {
		t.Errorf("filtered collision bound methods: byName=%d byID=%d, want 1/1", len(bindings.boundMethods), len(bindings.boundByID))
	}

	// Mirror case: deny A, allow B.
	mirror := NewBindings(nil, nil)
	filteredA := NewServiceWithOptions(&filterDupServiceA{}, ServiceOptions{
		MethodFilter: func(string) bool { return false },
	})
	if err := mirror.Add(filteredA); err != nil {
		t.Fatalf("Add(filtered A): %v", err)
	}
	if err := mirror.Add(NewService(&filterDupServiceB{})); err != nil {
		t.Fatalf("Add(B) after filtered A = %v, want nil", err)
	}
	if bm := mirror.Get(&CallOptions{MethodName: fqnA}); bm != nil {
		t.Errorf("Get(%q) = %v, want nil in mirror case", fqnA, bm)
	}
	bmB := mirror.Get(&CallOptions{MethodName: fqnB})
	if bmB == nil || bmB.ID != sharedID {
		t.Fatalf("mirror Get(%q) = %v, want method with registered ID %d", fqnB, bmB, sharedID)
	}
	if bm := mirror.GetByID(sharedID); bm != bmB {
		t.Errorf("mirror GetByID(%d) = %v, want allowed B %v", sharedID, bm, bmB)
	}
}
