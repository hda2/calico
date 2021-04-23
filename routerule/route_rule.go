// Copyright (c) 2020 Tigera, Inc. All rights reserved.
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

package routerule

import (
	"errors"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/libcalico-go/lib/set"

	"golang.org/x/sys/unix"
)

const (
	maxConnFailures = 3
)

var (
	GetFailed     = errors.New("netlink get operation failed")
	ConnectFailed = errors.New("connect to netlink failed")
	ListFailed    = errors.New("netlink list operation failed")
	UpdateFailed  = errors.New("netlink update operation failed")

	TableIndexFailed = errors.New("no table index specified")
)

// RouteRules represents set of routing rules with same ip family and priority.
// The target of those rules are set of routing tables.
type RouteRules struct {
	logCxt *log.Entry

	IPVersion int
	Priority  int

	// Routing table indexes which is exclusively managed by us.
	tableIndexSet set.Set

	netlinkFamily  int
	netlinkTimeout time.Duration
	// numConsistentNetlinkFailures counts the number of repeated netlink connection failures.
	// reset on successful connection.
	numConsistentNetlinkFailures int
	// Current netlink handle, or nil if we need to reconnect.
	cachedNetlinkHandle HandleIface

	// Rules match function for rule update.
	// For rule updates, it would generally need to match all fields concerned.
	// For example, egress ip manager considers two rules are matching if they
	// have same FWMark , source ip matching condition and go to same table index.
	matchForUpdate RulesMatchFunc
	// Rules match function for rule remove.
	// For rule remove, it would generally just to match some fields concerned.
	// For example, egress ip manager considers two rules are matching if they
	// have same FWMark , source ip matching condition.
	matchForRemove RulesMatchFunc

	// activeRules holds rules which should be programmed.
	activeRules set.Set
	inSync      bool

	// Testing shims, swapped with mock versions for UT
	newNetlinkHandle func() (HandleIface, error)
}

func New(ipVersion int, priority int, tableIndexSet set.Set, updateFunc, removeFunc RulesMatchFunc, netlinkTimeout time.Duration) (*RouteRules, error) {
	return NewWithShims(
		ipVersion,
		priority,
		tableIndexSet,
		updateFunc,
		removeFunc,
		netlinkTimeout,
		newNetlinkHandle,
	)
}

// NewWithShims is a test constructor, which allows netlink to be replaced by shims.
func NewWithShims(
	ipVersion int,
	priority int,
	tableIndexSet set.Set,
	updateFunc RulesMatchFunc,
	removeFunc RulesMatchFunc,
	netlinkTimeout time.Duration,
	newNetlinkHandle func() (HandleIface, error),
) (*RouteRules, error) {
	if tableIndexSet.Len() == 0 {
		return nil, TableIndexFailed
	}

	indexOK := true
	tableIndexSet.Iter(func(item interface{}) error {
		i := item.(int)
		if (i == 0) || (i >= unix.RT_TABLE_COMPAT) {
			indexOK = false
			return set.StopIteration
		}
		return nil
	})

	if !indexOK {
		return nil, TableIndexFailed
	}

	return &RouteRules{
		logCxt: log.WithFields(log.Fields{
			"ipVersion": ipVersion,
		}),
		IPVersion:        ipVersion,
		Priority:         priority,
		matchForUpdate:   updateFunc,
		matchForRemove:   removeFunc,
		tableIndexSet:    tableIndexSet,
		activeRules:      set.New(),
		netlinkFamily:    ipVersionToNetlinkFamily(ipVersion),
		newNetlinkHandle: newNetlinkHandle,
		netlinkTimeout:   netlinkTimeout,
	}, nil
}

// Return an active Rule if it matches a given Rule based on RulesMatchFunc.
// Return nil if no active Rule exists.
func (r *RouteRules) getActiveRule(rule *Rule, f RulesMatchFunc) *Rule {
	var active *Rule
	r.activeRules.Iter(func(item interface{}) error {
		p := item.(*Rule)
		if f(p, rule) {
			active = p
			return set.StopIteration
		}
		return nil
	})

	return active
}

// Set a Rule. Add to activeRules if it does not already exist based on matchForUpdate function.
func (r *RouteRules) SetRule(rule *Rule) {
	if !r.tableIndexSet.Contains(rule.nlRule.Table) {
		log.WithField("tableindex", rule.nlRule.Table).Panic("Unknown Table Index")
	}

	if r.getActiveRule(rule, r.matchForUpdate) == nil {
		r.activeRules.Add(rule)
		r.inSync = false
	}
}

// Remove a Rule. Do nothing if Rule not exists depends based on matchForRemove function.
func (r *RouteRules) RemoveRule(rule *Rule) {
	if p := r.getActiveRule(rule, r.matchForRemove); p != nil {
		r.activeRules.Discard(p)
		r.inSync = false
	}
}

func (r *RouteRules) QueueResync() {
	r.logCxt.Info("Queueing a resync of routing rules.")
	r.inSync = false
}

func (r *RouteRules) getNetlinkHandle() (HandleIface, error) {
	if r.cachedNetlinkHandle == nil {
		if r.numConsistentNetlinkFailures >= maxConnFailures {
			log.WithField("numFailures", r.numConsistentNetlinkFailures).Panic(
				"Repeatedly failed to connect to netlink.")
		}
		log.Info("Trying to connect to netlink")
		nlHandle, err := r.newNetlinkHandle()
		if err != nil {
			r.numConsistentNetlinkFailures++
			log.WithError(err).WithField("numFailures", r.numConsistentNetlinkFailures).Error(
				"Failed to connect to netlink")
			return nil, err
		}
		err = nlHandle.SetSocketTimeout(r.netlinkTimeout)
		if err != nil {
			r.numConsistentNetlinkFailures++
			log.WithError(err).WithField("numFailures", r.numConsistentNetlinkFailures).Error(
				"Failed to set netlink timeout")
			nlHandle.Delete()
			return nil, err
		}
		r.cachedNetlinkHandle = nlHandle
	}
	if r.numConsistentNetlinkFailures > 0 {
		log.WithField("numFailures", r.numConsistentNetlinkFailures).Info(
			"Connected to netlink after previous failures.")
		r.numConsistentNetlinkFailures = 0
	}
	return r.cachedNetlinkHandle, nil
}

func (r *RouteRules) closeNetlinkHandle() {
	if r.cachedNetlinkHandle == nil {
		return
	}
	r.cachedNetlinkHandle.Delete()
	r.cachedNetlinkHandle = nil
}

func (r *RouteRules) PrintCurrentRules() {
	log.WithField("count", r.activeRules.Len()).Info("summary of active rules")
	r.activeRules.Iter(func(item interface{}) error {
		p := item.(*Rule)
		p.LogCxt().Info("active rule")
		return nil
	})
}

func (r *RouteRules) Apply() error {
	if r.inSync {
		return nil
	}

	nl, err := r.getNetlinkHandle()
	if err != nil {
		r.logCxt.WithError(err).Error("Failed to connect to netlink, retrying...")
		return ConnectFailed
	}

	nlRules, err := nl.RuleList(r.netlinkFamily)
	if err != nil {
		r.logCxt.WithError(err).Error("Failed to list routing rules, retrying...")
		r.closeNetlinkHandle() // Defensive: force a netlink reconnection next time.
		return ListFailed
	}

	// Work out two sets, rules to add and rules to remove.
	toAdd := r.activeRules.Copy()
	toRemove := set.New()
	for _, nlRule := range nlRules {
		// Give each loop a fresh copy of nlRule since we would need to use pointer later.
		nlRule := nlRule
		if r.tableIndexSet.Contains(nlRule.Table) {
			// Table index of the rule is managed by us.
			// Be careful, do not use &nlRule below as it remain same value through iterations.
			dataplaneRule := FromNetlinkRule(&nlRule)
			if activeRule := r.getActiveRule(dataplaneRule, r.matchForUpdate); activeRule != nil {
				// rule exists both in activeRules and dataplaneRules.
				toAdd.Discard(activeRule)
			} else {
				toRemove.Add(dataplaneRule)
			}
		}
	}

	updatesFailed := false

	toRemove.Iter(func(item interface{}) error {
		rule := item.(*Rule)
		if err := nl.RuleDel(rule.nlRule); err != nil {
			rule.LogCxt().WithError(err).Warnf("Failed to remove rule from dataplane.")
			updatesFailed = true
		} else {
			rule.LogCxt().Debugf("Rule removed from dataplane.")
		}
		return nil
	})

	toAdd.Iter(func(item interface{}) error {
		rule := item.(*Rule)
		if err := nl.RuleAdd(rule.nlRule); err != nil {
			rule.LogCxt().WithError(err).Warnf("Failed to add rule from dataplane.")
			updatesFailed = true
			return nil
		} else {
			rule.LogCxt().Debugf("Rule added to dataplane.")
		}
		return nil
	})

	if updatesFailed {
		r.closeNetlinkHandle() // Defensive: force a netlink reconnection next time.
		return UpdateFailed
	}

	r.inSync = true
	return nil
}

func ipVersionToNetlinkFamily(ipVersion int) int {
	family := unix.AF_INET
	if ipVersion == 6 {
		family = unix.AF_INET6
	} else if ipVersion != 4 {
		log.WithField("ipVersion", ipVersion).Panic("Unknown IP version")
	}
	return family
}