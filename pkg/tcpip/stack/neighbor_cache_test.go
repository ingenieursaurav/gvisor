// Copyright 2019 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"bytes"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gvisor.dev/gvisor/pkg/sleep"
	"gvisor.dev/gvisor/pkg/tcpip"
)

const (
	entryStoreSize = 4 * neighborCacheSize

	// typicalLatency for an ARP or NDP packet to travel to a router and back.
	typicalLatency = time.Millisecond
)

var (
	// eventDiffOpts are the options passed to cmp.Diff to compare neighbor
	// entries. The UpdatedAt field is ignored due to a lack of a deterministic
	// method to predict the time that an event will be dispatched.
	entryDiffOpts = cmpopts.IgnoreFields(NeighborEntry{}, "UpdatedAt")
)

func newTestNeighborCache(nudDisp NUDDispatcher, config NUDConfigurations, clock Clock) *neighborCache {
	config.resetInvalidFields()
	c := &neighborCache{
		clock: clock,
		nic: &NIC{
			stack: &Stack{
				nudDisp: nudDisp,
			},
			id:     1,
			linkEP: &testLinkEndpoint{},
		},
		state: NewNUDState(config),
	}
	c.mu.cache = make(map[tcpip.Address]*neighborEntry, neighborCacheSize)
	return c
}

// testEntryStore generates a default set of IP to MAC addresses used for
// testing and allows for modification of link addresses to simulate a new
// neighbor in the network reusing an IP address.
type testEntryStore struct {
	mu      sync.RWMutex
	entries map[tcpip.Address]NeighborEntry
}

func toAddress(i int) tcpip.Address {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint8(1))
	binary.Write(buf, binary.BigEndian, uint8(0))
	binary.Write(buf, binary.BigEndian, uint16(i))
	return tcpip.Address(buf.String())
}

func toLinkAddress(i int) tcpip.LinkAddress {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint8(1))
	binary.Write(buf, binary.BigEndian, uint8(0))
	binary.Write(buf, binary.BigEndian, uint32(i))
	return tcpip.LinkAddress(buf.String())
}

func newTestEntryStore() *testEntryStore {
	entries := make(map[tcpip.Address]NeighborEntry)
	for i := 0; i < entryStoreSize; i++ {
		addr := toAddress(i)
		linkAddr := toLinkAddress(i)
		entries[addr] = NeighborEntry{
			Addr:      addr,
			LocalAddr: tcpip.Address("LocalAddr"),
			LinkAddr:  linkAddr,
		}
	}
	return &testEntryStore{
		entries: entries,
	}
}

func (s *testEntryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

func (s *testEntryStore) Entry(i int) NeighborEntry {
	addr := toAddress(i)
	entry, _ := s.EntryByAddr(tcpip.Address(addr))
	return entry
}

func (s *testEntryStore) EntryByAddr(addr tcpip.Address) (NeighborEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[addr]
	return entry, ok
}

func (s *testEntryStore) Entries() []NeighborEntry {
	entries := make([]NeighborEntry, 0, len(s.entries))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := 0; i < entryStoreSize; i++ {
		addr := toAddress(i)
		if entry, ok := s.entries[tcpip.Address(addr)]; ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (s *testEntryStore) Set(i int, linkAddr tcpip.LinkAddress) {
	addr := toAddress(i)
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[tcpip.Address(addr)]; ok {
		entry.LinkAddr = linkAddr
		s.entries[tcpip.Address(addr)] = entry
	}
}

// testLinkAddressResolver implements LinkAddressResolver to emulate sending a
// neighbor probe.
type testLinkAddressResolver struct {
	clock                Clock
	neigh                *neighborCache
	entries              *testEntryStore
	delay                time.Duration
	onLinkAddressRequest func()
}

var _ LinkAddressResolver = (*testLinkAddressResolver)(nil)

func (r *testLinkAddressResolver) LinkAddressRequest(addr, localAddr tcpip.Address, linkAddr tcpip.LinkAddress, linkEP LinkEndpoint) *tcpip.Error {
	r.clock.AfterFunc(nil, r.delay, func() { r.fakeRequest(addr) })
	if f := r.onLinkAddressRequest; f != nil {
		f()
	}
	return nil
}

func (r *testLinkAddressResolver) fakeRequest(addr tcpip.Address) {
	if e, ok := r.entries.EntryByAddr(addr); ok {
		r.neigh.HandleConfirmation(addr, e.LinkAddr, ReachabilityConfirmationFlags{
			Solicited: true,
			Override:  false,
			IsRouter:  false,
		})
	}
}

func (*testLinkAddressResolver) ResolveStaticAddress(addr tcpip.Address) (tcpip.LinkAddress, bool) {
	if addr == "broadcast" {
		return "mac_broadcast", true
	}
	return "", false
}

func (*testLinkAddressResolver) LinkAddressProtocol() tcpip.NetworkProtocolNumber {
	return 1
}

// testLinkEndpoint implements LinkEndpoint to validate the sending of probes
// and advertisements upon each certain NUD events.
type testLinkEndpoint struct {
	LinkEndpoint
}

var _ LinkEndpoint = (*testLinkEndpoint)(nil)

func (*testLinkEndpoint) MTU() uint32 {
	return 1500
}

func (*testLinkEndpoint) Capabilities() LinkEndpointCapabilities {
	return CapabilityResolutionRequired
}

func (*testLinkEndpoint) IsAttached() bool {
	return true
}

type entryEvent struct {
	nicID    tcpip.NICID
	address  tcpip.Address
	linkAddr tcpip.LinkAddress
	state    NeighborState
}

func TestCacheGetConfig(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)

	if got, want := neigh.config(), c; got != want {
		t.Errorf("neigh.config()=%+v, want %+v", got, want)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

func TestCacheSetConfig(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)

	c.BaseReachableTime = infiniteDuration
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1
	neigh.setConfig(c)

	if got, want := neigh.config(), c; got != want {
		t.Errorf("neigh.config()=%+v, want %+v", got, want)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

func TestCacheEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	a := store.Entry(0)
	_, _, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}

	clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()

	_, _, err = neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != nil {
		t.Errorf("got unexpected error: %v", err)
	}

	// No more events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

// TestCacheEntryNoLinkAddress verifies calling entry() without a
// LinkAddressResolver returns ErrNoLinkAddress.
func TestCacheEntryNoLinkAddress(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)
	store := newTestEntryStore()

	a := store.Entry(0)
	_, _, err := neigh.entry(a.Addr, a.LocalAddr, nil, nil)
	if err != tcpip.ErrNoLinkAddress {
		t.Errorf("neigh.entry(%q) should return ErrNoLinkAddress, got %v", a.Addr, err)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

func TestCacheRemoveEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1
	config.UnreachableTime = infiniteDuration // stay in failed

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	a := store.Entry(0)
	_, _, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}

	clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()

	neigh.removeEntry(a.Addr)
	clock.advanceAll()

	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	_, _, err = neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
}

type testContext struct {
	t       *testing.T
	clock   *fakeClock
	neigh   *neighborCache
	store   *testEntryStore
	linkRes *testLinkAddressResolver
	nudDisp *testNUDDispatcher
}

func newTestContext(t *testing.T, c NUDConfigurations) testContext {
	nudDisp := &testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(nudDisp, c, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	return testContext{
		t:       t,
		clock:   &clock,
		neigh:   neigh,
		store:   store,
		linkRes: linkRes,
		nudDisp: nudDisp,
	}
}

type overflowOptions struct {
	expectedDynamicEntryCount int
	startAtEntryIndex         int
}

func (c *testContext) overflowCache(opts overflowOptions) {
	// Fill the neighbor cache to capacity to verify the LRU eviction strategy is
	// working properly after the entry removal.
	offset := opts.startAtEntryIndex - opts.expectedDynamicEntryCount
	for i := opts.startAtEntryIndex; i < neighborCacheSize+offset; i++ {
		a := c.store.Entry(i)
		if _, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil); err != tcpip.ErrWouldBlock {
			c.t.Errorf("fill %d, neigh.entry(%q) should block, got %v", i, a.Addr, err)
		}
		c.clock.advanceAll()
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      a.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      a.Addr,
				LinkAddr:  a.LinkAddr,
				State:     Reachable,
			},
		}
		c.nudDisp.mu.Lock()
		if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
			c.t.Fatalf("got invalid events (-got, +want):\n%s", diff)
		}
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
	}

	// Keep adding more entries
	for i := neighborCacheSize + offset; i < c.store.Len(); i++ {
		a := c.store.Entry(i)
		_, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil)
		if err != tcpip.ErrWouldBlock {
			c.t.Errorf("overflow %d, neigh.entry(%q) should block, got %v", i, a.Addr, err)
		}
		c.clock.advanceAll()
		removedEntry := c.store.Entry(i - neighborCacheSize)
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      removedEntry.Addr,
				LinkAddr:  removedEntry.LinkAddr,
				State:     Reachable,
			},
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      a.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      a.Addr,
				LinkAddr:  a.LinkAddr,
				State:     Reachable,
			},
		}
		c.nudDisp.mu.Lock()
		if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
			c.t.Fatalf("got invalid events (-got, +want):\n%s", diff)
		}
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
	}

	// Expect to find only the most recent entries.
	for i := c.store.Len() - neighborCacheSize; i < c.store.Len(); i++ {
		a := c.store.Entry(i)
		e, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil)
		if err != nil {
			c.t.Errorf("check %d, neigh.entry(%q)=%q, got error: %v", i, a.Addr, e.LinkAddr, err)
		}
		if e.LinkAddr != a.LinkAddr {
			c.t.Errorf("check %d, neigh.entry(%q)=%q, want %q", i, a.Addr, e.LinkAddr, a.LinkAddr)
		}
	}

	// The earliest entries should no longer be in the cache.
	entries := c.neigh.entries()
	entryByAddr := mapByAddr(c.t, entries)
	for i := opts.startAtEntryIndex; i < c.store.Len()-neighborCacheSize; i++ {
		addr := c.store.Entry(i).Addr
		if _, ok := entryByAddr[addr]; ok {
			c.t.Errorf("check %d, neigh.entry(%q), got exists, want nonexistent", i, addr)
		}
	}

	// No more events should have been dispatched.
	c.nudDisp.mu.Lock()
	for _, event := range c.nudDisp.mu.events {
		c.t.Errorf("unexpected event: %s", event)
	}
	c.nudDisp.mu.Unlock()
}

// TestCacheOverflow verifies that the LRU cache eviction strategy respects the
// dynamic entry count.
func TestCacheOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1
	config.UnreachableTime = infiniteDuration // stay in failed

	c := newTestContext(t, config)
	c.overflowCache(overflowOptions{
		startAtEntryIndex:         0,
		expectedDynamicEntryCount: 0,
	})
}

// TestCacheRemoveEntryThenOverflow verifies that the LRU cache eviction
// strategy respects the dynamic entry count when an entry is removed.
func TestCacheRemoveEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(t, config)

	// Add a dynamic entry
	a := c.store.Entry(0)
	_, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		c.t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		c.t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	// Remove the entry
	c.neigh.removeEntry(a.Addr)
	c.clock.advanceAll()
	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		c.t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	c.overflowCache(overflowOptions{
		startAtEntryIndex:         0,
		expectedDynamicEntryCount: 0,
	})
}

// TestCacheDuplicateStaticEntryWithSameLinkAddress verifies that adding a
// duplicate static entry with the same link address does not dispatch any
// events.
func TestCacheDuplicateStaticEntryWithSameLinkAddress(t *testing.T) {
	config := DefaultNUDConfigurations()
	c := newTestContext(t, config)

	// Add a static entry
	a := c.store.Entry(0)
	staticLinkAddr := a.LinkAddr + "static"
	c.neigh.addStaticEntry(a.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		c.t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	// Remove the static entry that was just added
	c.neigh.addStaticEntry(a.Addr, staticLinkAddr)
	c.clock.advanceAll()

	// No more events should have been dispatched.
	c.nudDisp.mu.Lock()
	for _, event := range c.nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	c.nudDisp.mu.Unlock()
}

// TestCacheDuplicateStaticEntryWithDifferentLinkAddress verifies that adding a
// duplicate static entry with a different link address dispatches a change
// event.
func TestCacheDuplicateStaticEntryWithDifferentLinkAddress(t *testing.T) {
	config := DefaultNUDConfigurations()
	c := newTestContext(t, config)

	// Add a static entry
	a := c.store.Entry(0)
	staticLinkAddr := a.LinkAddr + "static"
	c.neigh.addStaticEntry(a.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	// Remove the static entry that was just added
	staticLinkAddr += "duplicate"
	c.neigh.addStaticEntry(a.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Errorf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.Unlock()
}

// TestCacheRemoveStaticEntryThenOverflow verifies that the LRU cache eviction
// strategy respects the dynamic entry count when a static entry is added then
// removed. In this case, the dynamic entry count shouldn't have been touched.
func TestCacheRemoveStaticEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(t, config)

	// Add a static entry
	a := c.store.Entry(0)
	staticLinkAddr := a.LinkAddr + "static"
	c.neigh.addStaticEntry(a.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	// Remove the static entry that was just added
	c.neigh.removeEntry(a.Addr)
	c.clock.advanceAll()
	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	c.overflowCache(overflowOptions{
		startAtEntryIndex:         0,
		expectedDynamicEntryCount: 0,
	})
}

// TestCacheOverwriteWithStaticEntryThenOverflow verifies that the LRU cache
// eviction strategy keeps count of the dynamic entry count when an entry is
// overwritten by a static entry. Static entries should not count towards the
// size of the LRU cache.
func TestCacheOverwriteWithStaticEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(t, config)

	// Add a dynamic entry
	a := c.store.Entry(0)
	_, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		c.t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	// Override the entry with a static one using the same address
	staticLinkAddr := a.LinkAddr + "static"
	c.neigh.addStaticEntry(a.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	c.overflowCache(overflowOptions{
		startAtEntryIndex:         1,
		expectedDynamicEntryCount: 0,
	})

	// The static entry should be in the cache.
	e, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil)
	if err != nil {
		c.t.Errorf("expected static neighbor entry for %q, got error: %v", a.Addr, err)
	}
	if got, want := e.LinkAddr, staticLinkAddr; got != want {
		c.t.Errorf("got e.LinkAddr=%q, want %q", got, want)
	}
	if got, want := e.State, Static; got != want {
		c.t.Errorf("got e.State=%q, want %q", got, want)
	}
	c.clock.advanceAll()

	// No more events should have been dispatched.
	c.nudDisp.mu.Lock()
	for _, event := range c.nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	c.nudDisp.mu.Unlock()
}

func TestCacheNotifiesWaker(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	w := sleep.Waker{}
	s := sleep.Sleeper{}
	const wakerID = 1
	s.AddWaker(&w, wakerID)

	a := store.Entry(0)
	_, doneCh, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, &w)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	if doneCh == nil {
		t.Fatalf("expected channel from neigh.entry(%q), got none", a.Addr)
	}
	clock.advanceAll()

	select {
	case <-doneCh:
	default:
		t.Fatal("expected notification from done channel")
	}

	id, ok := s.Fetch(false /* block */)
	if !ok {
		t.Error("expected waker to be notified")
	}
	if id != wakerID {
		t.Errorf("got s.Fetch(false)=%d, want=%d", id, wakerID)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Errorf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheRemoveWaker(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	w := sleep.Waker{}
	s := sleep.Sleeper{}
	const wakerID = 1
	s.AddWaker(&w, wakerID)

	a := store.Entry(0)
	_, doneCh, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, &w)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	if doneCh == nil {
		t.Fatalf("expected channel from neigh.entry(%q), got none", a.Addr)
	}

	// Remove the waker before the neighbor cache has the opportunity to send a
	// notification.
	neigh.removeWaker(a.Addr, &w)
	clock.advanceAll()

	select {
	case <-doneCh:
	default:
		t.Fatal("expected notification from done channel")
	}

	id, ok := s.Fetch(false /* block */)
	if ok {
		t.Error("unexpected notification from waker")
	}
	if id == wakerID {
		t.Errorf("got s.Fetch(false)=%d, want anything but %d", id, wakerID)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Errorf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheAddStaticEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(t, config)

	c.neigh.addStaticEntry(entryTestAddr1, entryTestLinkAddr1)
	e, _, err := c.neigh.entry(entryTestAddr1, "", nil, nil)
	if err != nil {
		t.Errorf("c.neigh.entry(%q) returned error %s", entryTestAddr1, err)
	}
	if got, want := e.LinkAddr, entryTestLinkAddr1; got != want {
		t.Errorf("got c.neigh.entry(%q).LinkAddr=%q, want %q", entryTestAddr1, got, want)
	}
	c.clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entryTestAddr1,
			LinkAddr:  entryTestLinkAddr1,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	c.overflowCache(overflowOptions{
		startAtEntryIndex:         0,
		expectedDynamicEntryCount: 0,
	})
}

func TestCacheRemoveStaticEntry(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	a := store.Entry(0)
	neigh.addStaticEntry(a.Addr, a.LinkAddr)
	clock.advanceAll()
	neigh.removeEntry(a.Addr)
	clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Static,
		},
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Static,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	_, _, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
}

func TestCacheStaticEntryOverridesDynamicEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	// Add a dynamic entry.
	a := store.Entry(0)
	_, _, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()

	e, _, err := neigh.entry(a.Addr, "", nil, nil)
	if err != nil {
		t.Errorf("neigh.entry(%q) returned error %s", a.Addr, err)
	}

	a.State = Reachable
	if diff := cmp.Diff(e, a, entryDiffOpts); diff != "" {
		t.Errorf("invalid neighbor entry received (-got, +want):\n%s", diff)
	}
	clock.advanceAll()

	// Replace the dynamic entry with a static one.
	neigh.addStaticEntry(a.Addr, entryTestLinkAddr1)
	clock.advanceAll()

	e, _, err = neigh.entry(a.Addr, "", nil, nil)
	if err != nil {
		t.Errorf("neigh.entry(%q) returned error %s", a.Addr, err)
	}

	wantEntry := NeighborEntry{
		Addr:     a.Addr,
		LinkAddr: entryTestLinkAddr1,
		State:    Static,
	}
	if diff := cmp.Diff(e, wantEntry, entryDiffOpts); diff != "" {
		t.Errorf("invalid neighbor entry received (-got, +want):\n%s", diff)
	}
	clock.advanceAll()

	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  entryTestLinkAddr1,
			State:     Static,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Errorf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheClear(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	// Add a dynamic entry.
	a := store.Entry(0)
	_, _, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()

	// Add a static entry.
	neigh.addStaticEntry(entryTestAddr1, entryTestLinkAddr1)
	clock.advanceAll()

	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entryTestAddr1,
			LinkAddr:  entryTestLinkAddr1,
			State:     Static,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()

	// Clear shoud remove both dynamic and static entries.
	neigh.clear()
	clock.advanceAll()

	// Remove events dispatched from clear() have no deterministic order.
	events := make(map[tcpip.Address]testEntryEventInfo)
	nudDisp.mu.Lock()
	if len(nudDisp.mu.events) < 2 {
		t.Fatalf("expected two more events, got %d", len(nudDisp.mu.events))
	}
	for i := 0; i < 2; i++ {
		e := nudDisp.mu.events[i]
		if existing, ok := events[e.Addr]; ok {
			if diff := cmp.Diff(existing, e); diff != "" {
				t.Fatalf("duplicate event found (-existing +got):\n%s", diff)
			} else {
				t.Fatalf("exact event duplicate found for %s", e)
			}
		}
		events[e.Addr] = e
	}
	nudDisp.mu.events = nudDisp.mu.events[2:]
	nudDisp.mu.Unlock()

	gotEvent, ok := events[a.Addr]
	if !ok {
		t.Fatalf("expected event with Addr=%q", a.Addr)
	}

	wantEvent := testEntryEventInfo{
		EventType: entryTestRemoved,
		NICID:     1,
		Addr:      a.Addr,
		LinkAddr:  a.LinkAddr,
		State:     Reachable,
	}
	if diff := cmp.Diff(gotEvent, wantEvent, eventDiffOpts); diff != "" {
		t.Errorf("invalid event received (-got, +want):\n%s", diff)
	}

	gotEvent, ok = events[entryTestAddr1]
	if !ok {
		t.Fatalf("expected event with Addr=%q", a.Addr)
	}

	wantEvent = testEntryEventInfo{
		EventType: entryTestRemoved,
		NICID:     1,
		Addr:      entryTestAddr1,
		LinkAddr:  entryTestLinkAddr1,
		State:     Static,
	}
	if diff := cmp.Diff(gotEvent, wantEvent, eventDiffOpts); diff != "" {
		t.Errorf("invalid event received (-got, +want):\n%s", diff)
	}

	// No more events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

func mapByAddr(t *testing.T, entries []NeighborEntry) map[tcpip.Address]NeighborEntry {
	t.Helper()
	entryByAddr := make(map[tcpip.Address]NeighborEntry)

	for _, e := range entries {
		if existing, ok := entryByAddr[e.Addr]; ok {
			if diff := cmp.Diff(existing, e); diff != "" {
				t.Fatalf("duplicate neighbor entry found (-existing +got):\n%s", diff)
			} else {
				t.Fatalf("exact neighbor entry duplicate found:\n%s", e)
			}
		}
		entryByAddr[e.Addr] = e
	}

	return entryByAddr
}

// TestCacheClearThenOverflow verifies that the LRU cache eviction strategy
// keeps count of the dynamic entry count when all entries are cleared.
func TestCacheClearThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(t, config)

	// Add a dynamic entry
	a := c.store.Entry(0)
	_, _, err := c.neigh.entry(a.Addr, a.LocalAddr, c.linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		c.t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      a.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	// Clear the cache.
	c.neigh.clear()
	c.clock.advanceAll()
	wantEvents = []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      a.Addr,
			LinkAddr:  a.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
		t.Fatalf("got invalid events (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()

	c.overflowCache(overflowOptions{
		startAtEntryIndex:         0,
		expectedDynamicEntryCount: 0,
	})
}

func TestCacheKeepFrequentlyUsed(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	frequentlyUsedEntry := store.Entry(0)

	// Fill the neighbor cache to capacity
	for i := 0; i < neighborCacheSize; i++ {
		a := store.Entry(i)
		_, doneCh, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
		if err != tcpip.ErrWouldBlock {
			t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
		}
		clock.advanceAll()
		select {
		case <-doneCh:
		default:
			t.Fatalf("expected notification from done channel returned by neigh.entry(%q)", a.Addr)
		}
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      a.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      a.Addr,
				LinkAddr:  a.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
			t.Fatalf("got invalid events (-got, +want):\n%s", diff)
		}
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
	}
	// Keep adding more entries
	for i := neighborCacheSize; i < store.Len(); i++ {
		// Periodically refresh the frequently used entry
		if i%(neighborCacheSize/2) == 0 {
			_, _, err := neigh.entry(frequentlyUsedEntry.Addr, frequentlyUsedEntry.LocalAddr, linkRes, nil)
			if err != nil {
				t.Errorf("got error while refreshing recently used entry: %v", err)
			}
		}

		a := store.Entry(i)
		_, doneCh, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
		if err != tcpip.ErrWouldBlock {
			t.Errorf("neigh.entry(%q) should block, got %v", a.Addr, err)
		}
		clock.advanceAll()
		select {
		case <-doneCh:
		default:
			t.Fatalf("expected notification from done channel returned by neigh.entry(%q)", a.Addr)
		}
		removedEntry := store.Entry(i - neighborCacheSize + 1)
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      removedEntry.Addr,
				LinkAddr:  removedEntry.LinkAddr,
				State:     Reachable,
			},
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      a.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      a.Addr,
				LinkAddr:  a.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
			t.Fatalf("got invalid events (-got, +want):\n%s", diff)
		}
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
	}
	// Expect to find only the most recent entries.
	for i := store.Len() - neighborCacheSize + 1; i < store.Len(); i++ {
		a := store.Entry(i)
		e, _, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
		if err != nil {
			t.Errorf("insert %d, neigh.entry(%q)=%q, got error: %v", i, a.Addr, e.LinkAddr, err)
		}
		if e.LinkAddr != a.LinkAddr {
			t.Errorf("insert %d, neigh.entry(%q)=%q, want %q", i, a.Addr, e.LinkAddr, a.LinkAddr)
		}
	}
	// The earliest entries should no longer be in the cache.
	entries := neigh.entries()
	entryByAddr := mapByAddr(t, entries)
	for i := 1; i < store.Len()-neighborCacheSize; i++ {
		addr := store.Entry(i).Addr
		if _, ok := entryByAddr[addr]; ok {
			t.Errorf("check %d, neigh.entry(%q), got exists, want nonexistent", i, addr)
		}
	}
	// The frequently used entry should be in the cache
	if _, ok := entryByAddr[frequentlyUsedEntry.Addr]; !ok {
		t.Error("expected frequently used entry to exist")
	}

	// No more events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

func TestCacheConcurrent(t *testing.T) {
	const concurrentProcesses = 16

	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	storeEntries := store.Entries()
	for i, e := range storeEntries {
		e := e // capture range variable
		for r := 0; r < concurrentProcesses; r++ {
			clock.AfterFunc(nil, time.Duration(r*concurrentProcesses+i)*time.Millisecond, func() {
				_, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil)
				if err != nil && err != tcpip.ErrWouldBlock {
					t.Errorf("neigh.entry(%q) want success or ErrWouldBlock, got %v", e.Addr, err)
				}
			})
		}
		clock.advanceAll()
	}

	entries := make(map[tcpip.Address]NeighborEntry)
	for _, e := range neigh.entries() {
		entries[e.Addr] = e
	}

	// All goroutines add in the same order and add more values than can fit in
	// the cache. Our eviction strategy requires that the last entries are
	// present, up to the size of the neighbor cache, and the rest are missing.
	for i := 0; i < neighborCacheSize; i++ {
		e := store.Entry(store.Len() - 1 - i)
		if entry, ok := entries[e.Addr]; ok {
			if entry.LinkAddr != e.LinkAddr {
				t.Errorf("neigh.entry(%q)=%q, want %q", e.Addr, entry.LinkAddr, e.LinkAddr)
			}
		} else {
			t.Errorf("neigh.entry(%q) does not exists, want exists", e.Addr)
		}
	}

	for i := 0; i < store.Len()-neighborCacheSize; i++ {
		e := store.Entry(i)
		if _, ok := entries[e.Addr]; ok {
			t.Errorf("neigh.entry(%q) exists, want does not exist", e.Addr)
		}
	}
}

func TestCacheReplace(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	// Add an entry
	a := store.Entry(0)
	e, doneCh, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	clock.advanceAll()
	select {
	case <-doneCh:
	default:
		t.Fatalf("expected notification from done channel returned by neigh.entry(%q)", a.Addr)
	}

	// Verify the entry exists
	e, doneCh, err = neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != nil {
		t.Fatalf("neigh.entry(%q) should not error, got %v", a.Addr, err)
	}
	if doneCh != nil {
		t.Fatalf("neigh.entry(%q) unexpectedly returned done channel", a.Addr)
	}
	if e.State != Reachable {
		t.Errorf("neigh.entry(%q).State = %q, want %q", a.Addr, e.State, Reachable)
	}
	if e.LinkAddr != a.LinkAddr {
		t.Errorf("neigh.entry(%q).LinkAddr = %q, want %q", a.Addr, e.LinkAddr, a.LinkAddr)
	}
	clock.advanceAll()

	// Notify of a link address change
	updatedLinkAddr := store.Entry(1).LinkAddr
	store.Set(0, updatedLinkAddr)
	neigh.HandleConfirmation(a.Addr, updatedLinkAddr, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	e, doneCh, err = neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) should block, got %v", a.Addr, err)
	}
	clock.advanceAll()
	select {
	case <-doneCh:
	default:
		t.Fatalf("expected notification from done channel returned by neigh.entry(%q)", a.Addr)
	}

	// Verify the entry's new link address
	e, _, err = neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
	clock.advanceAll()
	if err != nil {
		t.Errorf("neigh.entry(%q) should exist, got %v", a.Addr, err)
	}
	if e.LinkAddr != updatedLinkAddr {
		t.Errorf("neigh.entry(%q).LinkAddr = %q, want %q", a.Addr, e.LinkAddr, updatedLinkAddr)
	}
}

func TestCacheResolution(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	// Fill the neighbor cache to full capacity
	for i := 0; i < neighborCacheSize; i++ {
		e := store.Entry(i)
		if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
			t.Fatalf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrWouldBlock)
		}
		clock.advanceAll()
		got, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil)
		if err != nil {
			t.Errorf("unexpected error from neigh.entry(%q): %v", e.Addr, err)
		}
		if got.LinkAddr != e.LinkAddr {
			t.Errorf("got neigh.entry(%q)=%q, want %q", e.Addr, got.LinkAddr, e.LinkAddr)
		}
		clock.advanceAll()
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      e.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      e.Addr,
				LinkAddr:  e.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
			t.Fatalf("got invalid events (-got, +want):\n%s", diff)
		}
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
	}

	// Use the rest of the entries in the store
	for i := neighborCacheSize; i < store.Len(); i++ {
		e := store.Entry(i)
		if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
			t.Fatalf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrWouldBlock)
		}
		clock.advanceAll()
		got, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil)
		if err != nil {
			t.Errorf("unexpected error from neigh.entry(%q): %v", e.Addr, err)
		}
		if got.LinkAddr != e.LinkAddr {
			t.Errorf("got neigh.entry(%q)=%q, want %q", e.Addr, got.LinkAddr, e.LinkAddr)
		}
		clock.advanceAll()
		removedEntry := store.Entry(i - neighborCacheSize)
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      removedEntry.Addr,
				LinkAddr:  removedEntry.LinkAddr,
				State:     Reachable,
			},
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      e.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      e.Addr,
				LinkAddr:  e.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts); diff != "" {
			t.Fatalf("got invalid events (-got, +want):\n%s", diff)
		}
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
	}

	// Check that after resolved, address stays in the cache and never returns WouldBlock.
	for i := 0; i < neighborCacheSize; i++ {
		e := store.Entry(store.Len() - i - 1)
		got, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil)
		if err != nil {
			t.Errorf("neigh.entry(%q)=%q, got error: %v", e.Addr, got, err)
		}
		if got.LinkAddr != e.LinkAddr {
			t.Errorf("neigh.entry(%q)=%q, want %q", e.Addr, got.LinkAddr, e.LinkAddr)
		}
	}

	// No more events should have been dispatched.
	nudDisp.mu.Lock()
	for _, event := range nudDisp.mu.events {
		t.Errorf("unexpected event: %s", event)
	}
	nudDisp.mu.Unlock()
}

func TestCacheResolutionFailed(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1
	config.UnreachableTime = infiniteDuration // stay in failed

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()

	var requestCount uint32
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
		onLinkAddressRequest: func() {
			atomic.AddUint32(&requestCount, 1)
		},
	}

	// First, sanity check that resolution is working
	e := store.Entry(0)
	if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	got, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil)
	if err != nil {
		t.Errorf("unexpected error from neigh.entry(%q): %v", e.Addr, err)
	}
	if got.LinkAddr != e.LinkAddr {
		t.Errorf("got neigh.entry(%q)=%q, want %q", e.Addr, got.LinkAddr, e.LinkAddr)
	}
	clock.advanceAll()

	// Verify that address resolution for an unknown address returns ErrNoLinkAddress
	before := atomic.LoadUint32(&requestCount)

	e.Addr += "2"
	if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrNoLinkAddress {
		t.Errorf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrNoLinkAddress)
	}

	maxAttempts := neigh.config().MaxUnicastProbes
	if got, want := atomic.LoadUint32(&requestCount)-before, maxAttempts; got != want {
		t.Errorf("got link address request count = %d, want = %d", got, want)
	}
}

// TestCacheResolutionTimeout simulates sending MaxMulticastProbes probes and
// not retrieving a confirmation before the duration defined by
// MaxMulticastProbes * RetransmitTimer.
func TestCacheResolutionTimeout(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.RetransmitTimer = time.Millisecond // small enough to cause timeout
	config.UnreachableTime = infiniteDuration // stay in failed

	clock := fakeClock{}
	neigh := newTestNeighborCache(nil, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   time.Minute, // large enough to cause timeout
	}

	e := store.Entry(0)
	if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Fatalf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	if _, _, err := neigh.entry(e.Addr, e.LocalAddr, linkRes, nil); err != tcpip.ErrNoLinkAddress {
		t.Errorf("neigh.entry(%q) got error: %v, want error: %v", e.Addr, err, tcpip.ErrNoLinkAddress)
	}
}

// TestStaticResolution checks that static link addresses are resolved
// immediately and don't send resolution requests.
func TestStaticResolution(t *testing.T) {
	config := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(nil, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	addr := tcpip.Address("broadcast")
	localAddr := tcpip.Address("LocalAddr")
	want := tcpip.LinkAddress("mac_broadcast")
	got, _, err := neigh.entry(addr, localAddr, linkRes, nil)
	if err != nil {
		t.Errorf("neigh.entry(%q)=%q, got error: %v", addr, got, err)
	}
	if got.LinkAddr != want {
		t.Errorf("neigh.entry(%q)=%q, want %q", addr, got.LinkAddr, want)
	}
}

func BenchmarkCacheClear(b *testing.B) {
	b.StopTimer()
	config := DefaultNUDConfigurations()
	clock := systemClock{}
	neigh := newTestNeighborCache(nil, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   0,
	}

	// Clear for every possible size of the cache
	for cacheSize := 0; cacheSize < neighborCacheSize; cacheSize++ {
		// Fill the neighbor cache to capacity.
		for i := 0; i < cacheSize; i++ {
			a := store.Entry(i)
			_, doneCh, err := neigh.entry(a.Addr, a.LocalAddr, linkRes, nil)
			if err != tcpip.ErrWouldBlock {
				panic("expected entry to block")
			}
			if doneCh != nil {
				<-doneCh
			}
		}

		b.StartTimer()
		neigh.clear()
		b.StopTimer()
	}
}
