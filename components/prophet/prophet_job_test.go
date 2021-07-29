// Copyright 2020 MatrixOrigin.
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

package prophet

import (
	"sync"
	"testing"
	"time"

	"github.com/matrixorigin/matrixcube/components/prophet/config"
	"github.com/stretchr/testify/assert"
)

func TestJobTask(t *testing.T) {
	jobs := make(map[string]string)
	cb := func(k, v []byte) {
		jobs[string(k)] = string(v)
	}
	cluster := newTestClusterProphet(t, 3, func(c *config.Config) {
		c.JobCheckerDuration = time.Second
		c.JobHandler = cb
	})
	defer func() {
		for _, p := range cluster {
			p.Stop()
		}
	}()

	assert.NoError(t, cluster[1].GetStorage().PutJob([]byte("k1"), []byte("v1")))
	assert.NoError(t, cluster[1].GetStorage().PutJob([]byte("k2"), []byte("v2")))
	assert.NoError(t, cluster[1].GetStorage().PutJob([]byte("k3"), []byte("v3")))

	time.Sleep(time.Second * 2)
	assert.Equal(t, 3, len(jobs))
	assert.Equal(t, "v1", jobs["k1"])
	assert.Equal(t, "v2", jobs["k2"])
	assert.Equal(t, "v3", jobs["k3"])
}

func TestJobTaskRestart(t *testing.T) {
	p := newTestSingleProphet(t, func(c *config.Config) {
		c.JobCheckerDuration = time.Second
		c.JobHandler = func(k, v []byte) {}
		c.TestCtx = &sync.Map{}
	})
	defer p.Stop()

	time.Sleep(time.Second)
	v, _ := p.GetConfig().TestCtx.Load("jobTask")
	assert.Equal(t, "started", v)

	p.(*defaultProphet).stopJobTask()
	time.Sleep(time.Second)
	v, _ = p.GetConfig().TestCtx.Load("jobTask")
	assert.Equal(t, "stopped", v)

	p.(*defaultProphet).startJobTask()
	time.Sleep(time.Second)
	v, _ = p.GetConfig().TestCtx.Load("jobTask")
	assert.Equal(t, "started", v)
}
