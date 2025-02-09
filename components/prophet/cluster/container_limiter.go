// Copyright 2020 PingCAP, Inc.
// Modifications copyright (C) 2021 MatrixOrigin.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"sync"

	"github.com/matrixorigin/matrixcube/components/prophet/config"
	"github.com/matrixorigin/matrixcube/components/prophet/limit"
	"github.com/matrixorigin/matrixcube/components/prophet/pb/metapb"
	"github.com/matrixorigin/matrixcube/components/prophet/util"
)

// ContainerLimiter adjust the container limit dynamically
type ContainerLimiter struct {
	m       sync.RWMutex
	opt     *config.PersistOptions
	scene   map[limit.Type]*limit.Scene
	state   *State
	current LoadState
}

// NewContainerLimiter builds a container limiter object using the operator controller
func NewContainerLimiter(opt *config.PersistOptions) *ContainerLimiter {
	defaultScene := map[limit.Type]*limit.Scene{
		limit.AddPeer:    limit.DefaultScene(limit.AddPeer),
		limit.RemovePeer: limit.DefaultScene(limit.RemovePeer),
	}

	return &ContainerLimiter{
		opt:     opt,
		state:   NewState(),
		scene:   defaultScene,
		current: LoadStateNone,
	}
}

// Collect the container statistics and update the cluster state
func (s *ContainerLimiter) Collect(stats *metapb.ContainerStats) {
	s.m.Lock()
	defer s.m.Unlock()

	util.GetLogger().Debugf("collected statistics %+v", stats)
	s.state.Collect((*StatEntry)(stats))

	state := s.state.State()
	ratePeerAdd := s.calculateRate(limit.AddPeer, state)
	ratePeerRemove := s.calculateRate(limit.RemovePeer, state)

	if ratePeerAdd > 0 || ratePeerRemove > 0 {
		if ratePeerAdd > 0 {
			s.opt.SetAllContainersLimit(limit.AddPeer, ratePeerAdd)
			util.GetLogger().Infof("change container resource add limit for cluster, state %+v, rate %+v",
				state,
				ratePeerAdd)
		}
		if ratePeerRemove > 0 {
			s.opt.SetAllContainersLimit(limit.RemovePeer, ratePeerRemove)
			util.GetLogger().Infof("change container resource remove limit for cluster,  state %+v, rate %+v",
				state,
				ratePeerRemove)
		}
		s.current = state
		collectClusterStateCurrent(state)
	}
}

func collectClusterStateCurrent(state LoadState) {
	for i := LoadStateNone; i <= LoadStateHigh; i++ {
		if i == state {
			clusterStateCurrent.WithLabelValues(state.String()).Set(1)
			continue
		}
		clusterStateCurrent.WithLabelValues(i.String()).Set(0)
	}
}

func (s *ContainerLimiter) calculateRate(limitType limit.Type, state LoadState) float64 {
	rate := float64(0)
	switch state {
	case LoadStateIdle:
		rate = float64(s.scene[limitType].Idle)
	case LoadStateLow:
		rate = float64(s.scene[limitType].Low)
	case LoadStateNormal:
		rate = float64(s.scene[limitType].Normal)
	case LoadStateHigh:
		rate = float64(s.scene[limitType].High)
	}
	return rate
}

// ReplaceContainerLimitScene replaces the container limit values for different scenes
func (s *ContainerLimiter) ReplaceContainerLimitScene(scene *limit.Scene, limitType limit.Type) {
	s.m.Lock()
	defer s.m.Unlock()
	if s.scene == nil {
		s.scene = make(map[limit.Type]*limit.Scene)
	}
	s.scene[limitType] = scene
}

// ContainerLimitScene returns the current limit for different scenes
func (s *ContainerLimiter) ContainerLimitScene(limitType limit.Type) *limit.Scene {
	s.m.RLock()
	defer s.m.RUnlock()
	return s.scene[limitType]
}
