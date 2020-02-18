// Copyright 2020 The gVisor Authors.
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

package stack_test

import (
	"math"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	defaultBaseReachableTime           = 30 * time.Second
	minimumBaseReachableTime           = time.Millisecond
	defaultMinRandomFactor             = 0.5
	defaultMaxRandomFactor             = 1.5
	defaultRetransmitTimer             = time.Second
	minimumRetransmitTimer             = time.Millisecond
	defaultDelayFirstProbeTime         = 5 * time.Second
	defaultMaxMulticastProbes          = 3
	defaultMaxUnicastProbes            = 3
	defaultMaxAnycastDelayTime         = time.Second
	defaultMaxReachbilityConfirmations = 3
	defaultUnreachableTime             = 5 * time.Second
)

// TestSetNUDConfigurationFailsForBadNICID tests to make sure we get an error if
// we attempt to update NUD configurations using an invalid NICID.
func TestSetNUDConfigurationFailsForBadNICID(t *testing.T) {
	s := stack.New(stack.Options{
		// A neighbor cache is required to store NUDConfigurations. The networking
		// stack will only allocate neighbor caches if a protocol providing link
		// address resolution is specified (e.g. ARP or IPv6).
		NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
	})

	// No NIC with ID 1 yet.
	if got := s.SetNUDConfigurations(1, stack.NUDConfigurations{}); got != tcpip.ErrUnknownNICID {
		t.Fatalf("got s.SetNDPConfigurations = %v, want = %s", got, tcpip.ErrUnknownNICID)
	}
}

// TestNUDConfigurationFailsForNotSupported tests to make sure we get a
// NotSupported error if we attempt to retrieve NUD configurations when the
// stack doesn't support NUD.
//
// The stack will report to not support NUD if a neighbor cache for a given NIC
// is not allocated. The networking stack will only allocate neighbor caches if
// a protocol providing link address resolution is specified (e.g. ARP, IPv6).
func TestNUDConfigurationFailsForNotSupported(t *testing.T) {
	const nicID = 1
	const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

	e := channel.New(0, 1280, linkAddr)
	e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	s := stack.New(stack.Options{
		NUDConfigs: stack.DefaultNUDConfigurations(),
	})
	if err := s.CreateNIC(nicID, e); err != nil {
		t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
	}
	if _, err := s.NUDConfigurations(nicID); err != tcpip.ErrNotSupported {
		t.Fatalf("got s.NDPConfigurations = %v, want = %s", err, tcpip.ErrNotSupported)
	}
}

// TestNUDConfigurationFailsForNotSupported tests to make sure we get a
// NotSupported error if we attempt to set NUD configurations when the stack
// doesn't support NUD.
//
// The stack will report to not support NUD if a neighbor cache for a given NIC
// is not allocated. The networking stack will only allocate neighbor caches if
// a protocol providing link address resolution is specified (e.g. ARP, IPv6).
func TestSetNUDConfigurationFailsForNotSupported(t *testing.T) {
	const nicID = 1
	const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

	e := channel.New(0, 1280, linkAddr)
	e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	s := stack.New(stack.Options{
		NUDConfigs: stack.DefaultNUDConfigurations(),
	})
	if err := s.CreateNIC(nicID, e); err != nil {
		t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
	}
	if err := s.SetNUDConfigurations(nicID, stack.NUDConfigurations{}); err != tcpip.ErrNotSupported {
		t.Fatalf("got s.SetNDPConfigurations = %v, want = %s", err, tcpip.ErrNotSupported)
	}
}

// TestDefaultNUDConfigurationIsValid verifies that calling
// resetInvalidFields() on the result of DefaultNUDConfigurations() does not
// change anything. DefaultNUDConfigurations() should return a valid
// NUDConfigurations.
func TestDefaultNUDConfigurations(t *testing.T) {
	const nicID = 1
	const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

	e := channel.New(0, 1280, linkAddr)
	e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

	s := stack.New(stack.Options{
		// A neighbor cache is required to store NUDConfigurations. The networking
		// stack will only allocate neighbor caches if a protocol providing link
		// address resolution is specified (e.g. ARP or IPv6).
		NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
		NUDConfigs:       stack.DefaultNUDConfigurations(),
	})
	if err := s.CreateNIC(nicID, e); err != nil {
		t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
	}
	c, err := s.NUDConfigurations(nicID)
	if err != nil {
		t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
	}
	if got, want := c, stack.DefaultNUDConfigurations(); got != want {
		t.Errorf("got stack.NUDConfigurations(%d)=%v, want %v", nicID, got, want)
	}
}

func TestNUDConfigurationsBaseReachableTime(t *testing.T) {
	tests := []struct {
		name              string
		baseReachableTime time.Duration
		want              time.Duration
	}{
		// Invalid cases
		{
			name:              "EqualToZero",
			baseReachableTime: 0,
			want:              defaultBaseReachableTime,
		},
		// Valid cases
		{
			name:              "MoreThanZero",
			baseReachableTime: time.Millisecond,
			want:              time.Millisecond,
		},
		{
			name:              "MoreThanDefaultBaseReachableTime",
			baseReachableTime: 2 * defaultBaseReachableTime,
			want:              2 * defaultBaseReachableTime,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.BaseReachableTime = test.baseReachableTime

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.BaseReachableTime; got != test.want {
				t.Errorf("got BaseReachableTime=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsMinRandomFactor(t *testing.T) {
	tests := []struct {
		name            string
		minRandomFactor float32
		want            float32
	}{
		// Invalid cases
		{
			name:            "LessThanZero",
			minRandomFactor: -1,
			want:            defaultMinRandomFactor,
		},
		{
			name:            "EqualToZero",
			minRandomFactor: 0,
			want:            defaultMinRandomFactor,
		},
		// Valid cases
		{
			name:            "MoreThanZero",
			minRandomFactor: 1,
			want:            1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.MinRandomFactor = test.minRandomFactor

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.MinRandomFactor; got != test.want {
				t.Errorf("got MinRandomFactor=%f, want %f", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsMaxRandomFactor(t *testing.T) {
	tests := []struct {
		name            string
		minRandomFactor float32
		maxRandomFactor float32
		want            float32
	}{
		// Invalid cases
		{
			name:            "LessThanZero",
			minRandomFactor: defaultMinRandomFactor,
			maxRandomFactor: -1,
			want:            defaultMaxRandomFactor,
		},
		{
			name:            "EqualToZero",
			minRandomFactor: defaultMinRandomFactor,
			maxRandomFactor: 0,
			want:            defaultMaxRandomFactor,
		},
		{
			name:            "LessThanMinRandomFactor",
			minRandomFactor: defaultMinRandomFactor,
			maxRandomFactor: defaultMinRandomFactor * 0.99,
			want:            defaultMaxRandomFactor,
		},
		{
			name:            "MoreThanMinRandomFactorWhenMinRandomFactorIsLargerThanMaxRandomFactorDefault",
			minRandomFactor: defaultMaxRandomFactor * 2,
			maxRandomFactor: defaultMaxRandomFactor,
			want:            defaultMaxRandomFactor * 6,
		},
		// Valid cases
		{
			name:            "EqualToMinRandomFactor",
			minRandomFactor: defaultMinRandomFactor,
			maxRandomFactor: defaultMinRandomFactor,
			want:            defaultMinRandomFactor,
		},
		{
			name:            "MoreThanMinRandomFactor",
			minRandomFactor: defaultMinRandomFactor,
			maxRandomFactor: defaultMinRandomFactor * 1.1,
			want:            defaultMinRandomFactor * 1.1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.MinRandomFactor = test.minRandomFactor
			c.MaxRandomFactor = test.maxRandomFactor

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.MaxRandomFactor; got != test.want {
				t.Errorf("got MaxRandomFactor=%f, want %f", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsRetransmitTimer(t *testing.T) {
	tests := []struct {
		name            string
		retransmitTimer time.Duration
		want            time.Duration
	}{
		// Invalid cases
		{
			name:            "EqualToZero",
			retransmitTimer: 0,
			want:            defaultRetransmitTimer,
		},
		{
			name:            "LessThanMinimumRetransmitTimer",
			retransmitTimer: minimumRetransmitTimer - time.Nanosecond,
			want:            defaultRetransmitTimer,
		},
		// Valid cases
		{
			name:            "EqualToMinimumRetransmitTimer",
			retransmitTimer: minimumRetransmitTimer,
			want:            minimumBaseReachableTime,
		},
		{
			name:            "LargetThanMinimumRetransmitTimer",
			retransmitTimer: 2 * minimumBaseReachableTime,
			want:            2 * minimumBaseReachableTime,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.RetransmitTimer = test.retransmitTimer

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.RetransmitTimer; got != test.want {
				t.Errorf("got RetransmitTimer=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsDelayFirstProbeTime(t *testing.T) {
	tests := []struct {
		name                string
		delayFirstProbeTime time.Duration
		want                time.Duration
	}{
		// Invalid cases
		{
			name: "EqualToZero", delayFirstProbeTime: 0, want: defaultDelayFirstProbeTime,
		},
		// Valid cases
		{
			name: "MoreThanZero", delayFirstProbeTime: time.Millisecond, want: time.Millisecond,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.DelayFirstProbeTime = test.delayFirstProbeTime

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.DelayFirstProbeTime; got != test.want {
				t.Errorf("got DelayFirstProbeTime=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsMaxMulticastProbes(t *testing.T) {
	tests := []struct {
		name               string
		maxMulticastProbes uint32
		want               uint32
	}{
		// Invalid cases
		{
			name:               "EqualToZero",
			maxMulticastProbes: 0,
			want:               defaultMaxMulticastProbes,
		},
		// Valid cases
		{
			name:               "MoreThanZero",
			maxMulticastProbes: 1,
			want:               1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.MaxMulticastProbes = test.maxMulticastProbes

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.MaxMulticastProbes; got != test.want {
				t.Errorf("got MaxMulticastProbes=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsMaxUnicastProbes(t *testing.T) {
	tests := []struct {
		name             string
		maxUnicastProbes uint32
		want             uint32
	}{
		// Invalid cases
		{
			name:             "EqualToZero",
			maxUnicastProbes: 0,
			want:             defaultMaxUnicastProbes,
		},
		// Valid cases
		{
			name:             "MoreThanZero",
			maxUnicastProbes: 1,
			want:             1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.MaxUnicastProbes = test.maxUnicastProbes

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.MaxUnicastProbes; got != test.want {
				t.Errorf("got MaxUnicastProbes=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNUDConfigurationsUnreachableTime(t *testing.T) {
	tests := []struct {
		name            string
		unreachableTime time.Duration
		want            time.Duration
	}{
		// Invalid cases
		{
			name:            "EqualToZero",
			unreachableTime: 0,
			want:            defaultUnreachableTime,
		},
		// Valid cases
		{
			name:            "MoreThanZero",
			unreachableTime: time.Millisecond,
			want:            time.Millisecond,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const nicID = 1
			const linkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")

			c := stack.DefaultNUDConfigurations()
			c.UnreachableTime = test.unreachableTime

			e := channel.New(0, 1280, linkAddr)
			e.LinkEPCapabilities |= stack.CapabilityResolutionRequired

			s := stack.New(stack.Options{
				// A neighbor cache is required to store NUDConfigurations. The networking
				// stack will only allocate neighbor caches if a protocol providing link
				// address resolution is specified (e.g. ARP or IPv6).
				NetworkProtocols: []stack.NetworkProtocol{ipv6.NewProtocol()},
				NUDConfigs:       c,
			})
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			sc, err := s.NUDConfigurations(nicID)
			if err != nil {
				t.Fatalf("stack.NUDConfigurations(%d) = %s", nicID, err)
			}
			if got := sc.UnreachableTime; got != test.want {
				t.Errorf("got UnreachableTime=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNUDStateRecomputeReachableTime(t *testing.T) {
	tests := []struct {
		name              string
		baseReachableTime time.Duration
		minRandomFactor   float32
		maxRandomFactor   float32
		wantMin           time.Duration
		wantMax           time.Duration
	}{
		{
			name:              "AllZeros",
			baseReachableTime: 0,
			minRandomFactor:   0,
			maxRandomFactor:   0,
			wantMin:           0,
			wantMax:           0,
		},
		{
			name:              "ZeroMaxRandomFactor",
			baseReachableTime: time.Second,
			minRandomFactor:   0,
			maxRandomFactor:   0,
			wantMin:           0,
			wantMax:           0,
		},
		{
			name:              "ZeroMinRandomFactor",
			baseReachableTime: time.Second,
			minRandomFactor:   0,
			maxRandomFactor:   1,
			wantMin:           0,
			wantMax:           time.Second,
		},
		{
			name:              "MinAndMaxRandomFactorsEqual",
			baseReachableTime: time.Second,
			minRandomFactor:   1,
			maxRandomFactor:   1,
			wantMin:           time.Second,
			wantMax:           time.Second,
		},
		{
			name:              "MinAndMaxRandomFactorsDifferent",
			baseReachableTime: time.Second,
			minRandomFactor:   1,
			maxRandomFactor:   2,
			wantMin:           time.Second,
			wantMax:           2 * time.Second,
		},
		{
			name:              "MaxInt64",
			baseReachableTime: time.Duration(math.MaxInt64),
			minRandomFactor:   1,
			maxRandomFactor:   1,
			wantMin:           time.Duration(math.MaxInt64),
			wantMax:           time.Duration(math.MaxInt64),
		},
		{
			name:              "Overflow",
			baseReachableTime: time.Duration(math.MaxInt64),
			minRandomFactor:   1.5,
			maxRandomFactor:   1.5,
			wantMin:           time.Duration(math.MaxInt64),
			wantMax:           time.Duration(math.MaxInt64),
		},
		{
			name:              "DoubleOverflow",
			baseReachableTime: time.Duration(math.MaxInt64),
			minRandomFactor:   2.5,
			maxRandomFactor:   2.5,
			wantMin:           time.Duration(math.MaxInt64),
			wantMax:           time.Duration(math.MaxInt64),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := stack.NUDConfigurations{
				BaseReachableTime: test.baseReachableTime,
				MinRandomFactor:   test.minRandomFactor,
				MaxRandomFactor:   test.maxRandomFactor,
			}
			s := stack.NewNUDState(c)
			if got := s.ReachableTime(); got < test.wantMin || got > test.wantMax {
				t.Errorf("got ReachableTime=%q, want %q ≤ %q ≤ %q", got, test.wantMin, got, test.wantMax)
			}
		})
	}
}

// TestNUDStateReachableTime exercises the ReachableTime function to
// automatically recompute reachable time when the min/max random factors or
// base reachable time changes.
func TestNUDStateReachableTime(t *testing.T) {
	c := stack.DefaultNUDConfigurations()
	c.MinRandomFactor = 2 * defaultMaxRandomFactor
	c.MaxRandomFactor = 3 * defaultMaxRandomFactor
	c.BaseReachableTime = time.Second

	s := stack.NewNUDState(c)
	old := s.ReachableTime()

	if got, want := s.ReachableTime(), old; got != want {
		t.Errorf("got ReachableTime=%q, want %q", got, want)
	}

	// Check for recomputation when changing the min and max random factor. Both
	// need to be set at the same time so the ranges don't overlap.
	c.MinRandomFactor = 4 * defaultMaxRandomFactor
	c.MaxRandomFactor = 5 * defaultMaxRandomFactor
	s.SetConfig(c)

	if got, notWant := s.ReachableTime(), old; got == notWant {
		t.Errorf("got ReachableTime=%q, want!=%q", got, notWant)
	}

	// Should also recompute when base reachable time changes. The ranges don't
	// overlap, so there's no need to change the min and max random factor.
	old = s.ReachableTime()
	c.BaseReachableTime = time.Minute
	s.SetConfig(c)

	if got, notWant := s.ReachableTime(), old; got == notWant {
		t.Errorf("got ReachableTime=%q, want!=%q", got, notWant)
	}
}
