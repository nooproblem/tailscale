// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package monitor provides facilities for monitoring network
// interface and route changes. It primarily exists to know when
// portable devices move between different networks.
package monitor

import (
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"time"

	"inet.af/netaddr"
	"tailscale.com/net/interfaces"
	"tailscale.com/types/logger"
)

// pollWallTimeInterval is how often we check the time to check
// for big jumps in wall (non-monotonic) time as a backup mechanism
// to get notified of a sleeping device waking back up.
// Usually there are also minor network change events on wake that let
// us check the wall time sooner than this.
const pollWallTimeInterval = 15 * time.Second

// message represents a message returned from an osMon.
type message interface {
	// Ignore is whether we should ignore this message.
	ignore() bool
}

// osMon is the interface that each operating system-specific
// implementation of the link monitor must implement.
type osMon interface {
	Close() error

	// Receive returns a new network interface change message. It
	// should block until there's either something to return, or
	// until the osMon is closed. After a Close, the returned
	// error is ignored.
	Receive() (message, error)
}

// ChangeFunc is a callback function that's called when the network
// changed. The changed parameter is whether the network changed
// enough for interfaces.State to have changed since the last
// callback.
type ChangeFunc func(changed bool, state *interfaces.State)

// An allocated callbackHandle's address is the Mon.cbs map key.
type callbackHandle byte

// Mon represents a monitoring instance.
type Mon struct {
	logf   logger.Logf
	om     osMon // nil means not supported on this platform
	change chan struct{}
	stop   chan struct{} // closed on Stop

	mu         sync.Mutex // guards all following fields
	cbs        map[*callbackHandle]ChangeFunc
	ruleDelCB  map[*callbackHandle]RuleDeleteCallback
	ifState    *interfaces.State
	gwValid    bool       // whether gw and gwSelfIP are valid
	gw         netaddr.IP // our gateway's IP
	gwSelfIP   netaddr.IP // our own IP address (that corresponds to gw)
	started    bool
	closed     bool
	goroutines sync.WaitGroup
	wallTimer  *time.Timer // nil until Started; re-armed AfterFunc per tick
	lastWall   time.Time
	timeJumped bool // whether we need to send a changed=true after a big time jump
}

// New instantiates and starts a monitoring instance.
// The returned monitor is inactive until it's started by the Start method.
// Use RegisterChangeCallback to get notified of network changes.
func New(logf logger.Logf) (*Mon, error) {
	logf = logger.WithPrefix(logf, "monitor: ")
	m := &Mon{
		logf:     logf,
		cbs:      map[*callbackHandle]ChangeFunc{},
		change:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
		lastWall: wallTime(),
	}
	st, err := m.interfaceStateUncached()
	if err != nil {
		return nil, err
	}
	m.ifState = st

	m.om, err = newOSMon(logf, m)
	if err != nil {
		return nil, err
	}
	if m.om == nil {
		return nil, errors.New("newOSMon returned nil, nil")
	}

	return m, nil
}

// InterfaceState returns the state of the machine's network interfaces,
// without any Tailscale ones.
func (m *Mon) InterfaceState() *interfaces.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ifState
}

func (m *Mon) interfaceStateUncached() (*interfaces.State, error) {
	return interfaces.GetState()
}

// GatewayAndSelfIP returns the current network's default gateway, and
// the machine's default IP for that gateway.
//
// It's the same as interfaces.LikelyHomeRouterIP, but it caches the
// result until the monitor detects a network change.
func (m *Mon) GatewayAndSelfIP() (gw, myIP netaddr.IP, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gwValid {
		return m.gw, m.gwSelfIP, true
	}
	gw, myIP, ok = interfaces.LikelyHomeRouterIP()
	if ok {
		m.gw, m.gwSelfIP, m.gwValid = gw, myIP, true
	}
	return gw, myIP, ok
}

// RegisterChangeCallback adds callback to the set of parties to be
// notified (in their own goroutine) when the network state changes.
// To remove this callback, call unregister (or close the monitor).
func (m *Mon) RegisterChangeCallback(callback ChangeFunc) (unregister func()) {
	handle := new(callbackHandle)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cbs[handle] = callback
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.cbs, handle)
	}
}

// RuleDeleteCallback is a callback when a Linux IP policy routing
// rule is deleted. The table is the table number (52, 253, 354) and
// priority is the priority order number (for Tailscale rules
// currently: 5210, 5230, 5250, 5270)
type RuleDeleteCallback func(table uint8, priority uint32)

// RegisterRuleDeleteCallback adds callback to the set of parties to be
// notified (in their own goroutine) when a Linux ip rule is deleted.
// To remove this callback, call unregister (or close the monitor).
func (m *Mon) RegisterRuleDeleteCallback(callback RuleDeleteCallback) (unregister func()) {
	handle := new(callbackHandle)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ruleDelCB == nil {
		m.ruleDelCB = map[*callbackHandle]RuleDeleteCallback{}
	}
	m.ruleDelCB[handle] = callback
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.ruleDelCB, handle)
	}
}

// Start starts the monitor.
// A monitor can only be started & closed once.
func (m *Mon) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true

	switch runtime.GOOS {
	case "ios", "android":
		// For battery reasons, and because these platforms
		// don't really sleep in the same way, don't poll
		// for the wall time to detect for wake-for-sleep
		// walltime jumps.
	default:
		m.wallTimer = time.AfterFunc(pollWallTimeInterval, m.pollWallTime)
	}

	if m.om == nil {
		return
	}
	m.goroutines.Add(2)
	go m.pump()
	go m.debounce()
}

// Close closes the monitor.
func (m *Mon) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.stop)

	if m.wallTimer != nil {
		m.wallTimer.Stop()
	}

	var err error
	if m.om != nil {
		err = m.om.Close()
	}

	started := m.started
	m.mu.Unlock()

	if started {
		m.goroutines.Wait()
	}
	return err
}

// InjectEvent forces the monitor to pretend there was a network
// change and re-check the state of the network. Any registered
// ChangeFunc callbacks will be called within the event coalescing
// period (under a fraction of a second).
func (m *Mon) InjectEvent() {
	select {
	case m.change <- struct{}{}:
	default:
		// Another change signal is already
		// buffered. Debounce will wake up soon
		// enough.
	}
}

func (m *Mon) stopped() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

// pump continuously retrieves messages from the connection, notifying
// the change channel of changes, and stopping when a stop is issued.
func (m *Mon) pump() {
	defer m.goroutines.Done()
	for !m.stopped() {
		msg, err := m.om.Receive()
		if err != nil {
			if m.stopped() {
				return
			}
			// Keep retrying while we're not closed.
			m.logf("error from link monitor: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if rdm, ok := msg.(ipRuleDeletedMessage); ok {
			m.notifyRuleDeleted(rdm)
			continue
		}
		if msg.ignore() {
			continue
		}
		m.InjectEvent()
	}
}

func (m *Mon) notifyRuleDeleted(rdm ipRuleDeletedMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cb := range m.ruleDelCB {
		go cb(rdm.table, rdm.priority)
	}
}

// debounce calls the callback function with a delay between events
// and exits when a stop is issued.
func (m *Mon) debounce() {
	defer m.goroutines.Done()
	for {
		select {
		case <-m.stop:
			return
		case <-m.change:
		}

		if curState, err := m.interfaceStateUncached(); err != nil {
			m.logf("interfaces.State: %v", err)
		} else {
			m.mu.Lock()

			// See if we have a queued or new time jump signal.
			m.checkWallTimeAdvanceLocked()
			timeJumped := m.timeJumped
			if timeJumped {
				m.logf("time jumped (probably wake from sleep); synthesizing major change event")
			}

			oldState := m.ifState
			ifChanged := !curState.EqualFiltered(oldState, interfaces.UseInterestingInterfaces, interfaces.UseInterestingIPs)
			if ifChanged {
				m.gwValid = false
				m.ifState = curState

				if s1, s2 := oldState.String(), curState.String(); s1 == s2 {
					m.logf("[unexpected] network state changed, but stringification didn't: %v\nold: %s\nnew: %s\n", s1,
						jsonSummary(oldState), jsonSummary(curState))
				}
			}
			changed := ifChanged || timeJumped
			if changed {
				m.timeJumped = false
			}
			for _, cb := range m.cbs {
				go cb(changed, m.ifState)
			}
			m.mu.Unlock()
		}

		select {
		case <-m.stop:
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func jsonSummary(x interface{}) interface{} {
	j, err := json.Marshal(x)
	if err != nil {
		return err
	}
	return j
}

func wallTime() time.Time {
	// From time package's docs: "The canonical way to strip a
	// monotonic clock reading is to use t = t.Round(0)."
	return time.Now().Round(0)
}

func (m *Mon) pollWallTime() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.checkWallTimeAdvanceLocked()
	if m.timeJumped {
		m.InjectEvent()
	}
	m.wallTimer.Reset(pollWallTimeInterval)
}

// checkWallTimeAdvanceLocked updates m.timeJumped, if wall time jumped
// more than 150% of pollWallTimeInterval, indicating we probably just
// came out of sleep.
func (m *Mon) checkWallTimeAdvanceLocked() {
	now := wallTime()
	if now.Sub(m.lastWall) > pollWallTimeInterval*3/2 {
		m.timeJumped = true
	}
	m.lastWall = now
}

type ipRuleDeletedMessage struct {
	table    uint8
	priority uint32
}

func (ipRuleDeletedMessage) ignore() bool { return true }
